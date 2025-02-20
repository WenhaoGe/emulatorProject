package plugins

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"math/rand"
	"os"
	"strconv"
	"sync"
	"time"

	"bitbucket-eng-sjc1.cisco.com/an/apollo-test/dcSpewer/utils"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adconnector"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adio"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adlog"
	"go.uber.org/zap"
)

// Rate at which to run jobs. Can be used to speed up job output rate.
var jobRate time.Duration

var taskCacheMu sync.Mutex
var taskCache map[string][]byte

func init() {
	jobRateRaw := os.Getenv("JOB_RATE")
	if jobRateRaw != "" {
		jobRateSec, err := strconv.Atoi(jobRateRaw)
		if err != nil {
			panic(err)
		}

		jobRate = time.Millisecond * time.Duration(jobRateSec)
	}
	taskCache = make(map[string][]byte)

	jrunner = &jobRunner{
		wakeup: make(chan struct{}, 1),
		plugs:  make(map[string]*JobPlugin),
	}
}

type jobMessage struct {
	Name          string
	TaskType      string
	Interval      int64
	IntervalUnit  int64
	Task          []byte
	ResponseTopic string
	UpdateRunning bool
}

// Job plugin receives jobs to run and periodically calls other plugins and emits async responses.
// TODO: Support error induction
type JobPlugin struct {
	Platform *PluginManager
	mu       sync.Mutex
	jobs     map[string]*job
	Nodeid   string

	wakeupTimes map[time.Duration]time.Time
}

type jobRunner struct {
	mu      sync.Mutex
	running bool
	wakeup  chan struct{}

	plugs map[string]*JobPlugin
}

var jrunner *jobRunner

type jobResponse struct {
	JobName string
	Body    string
	Error   string
}

type job struct {
	ctx           context.Context
	name          string
	task          []byte
	plugin        Plugin
	responseTopic string

	interval time.Duration
	firstRun bool

	cannedResponse io.Reader

	runs int
}

func (j *job) execute(ctx context.Context) {
	logger := adlog.MustFromContext(ctx)
	utils.DoingWork(ctx, utils.InventoryWork)
	defer utils.DoneWork(ctx, utils.InventoryWork)
	defer func() { j.runs++ }()

	logger.Sugar().Debugf("%s job is running", j.name)
	var out io.Reader
	var err error
	if j.cannedResponse != nil {
		adlog.MustFromContext(ctx).Sugar().Infof("job sending back canned response")
		out = j.cannedResponse
	} else {
		out, err = j.plugin.Delegate(ctx, j.task)
	}

	jobRsp := &jobResponse{JobName: j.name}

	if err != nil {
		jobRsp.Error = err.Error()
	} else if out != nil {
		switch out := out.(type) {
		case *bytes.Buffer:
			jobRsp.Body = out.String()
		default:
			bbody, rerr := ioutil.ReadAll(out)
			if rerr != nil {
				logger.Error("failed to read job body", zap.Error(rerr))
				return
			}
			jobRsp.Body = string(bbody)
		}
	} else {
		logger.Info("nothing to send, skipping message transfer...")
		return
	}

	outbuf := new(bytes.Buffer)
	enc := json.NewEncoder(outbuf)
	err = enc.Encode(jobRsp)
	if err != nil {
		logger.Error("failed to encode job message", zap.Error(err))
		return
	}

	jobMsg, err := utils.GetMessageReader(ctx, adconnector.Header{
		Type:      "TypeJobStim",
		SessionId: j.responseTopic,
	}, outbuf)
	if err != nil {
		logger.Error("failed to create job message", zap.Error(err))
		return
	}

	getDispatcher(ctx).Dispatch(ctx, jobMsg)
}

func (p *jobRunner) run(ctx context.Context) {
	logger := adlog.MustFromContext(ctx)
	trace := utils.GetTraceFromContext(ctx)

	defer func() {
		p.mu.Lock()
		p.running = false
		p.mu.Unlock()
	}()

	sleepInt := time.Duration(math.MaxInt64)
	timer := time.NewTimer(sleepInt)
	defer timer.Stop()

	for {
		sleepInt = time.Duration(math.MaxInt64)
		// Get next time to wakeup
		p.mu.Lock()
		for _, plug := range p.plugs {
			for _, wakeuptime := range plug.wakeupTimes {
				if time.Until(wakeuptime) < sleepInt {
					sleepInt = time.Until(wakeuptime)
				}
			}
		}
		p.mu.Unlock()

		logger.Sugar().Infof("sleeper sleeping for %s, node: %s", sleepInt.String(), trace.Node)
		if sleepInt > time.Second {
			timer.Reset(sleepInt)

			select {
			case <-timer.C:
			case <-p.wakeup:
			case <-ctx.Done():
				return
			}
		} else {
			logger.Sugar().Infof("running all jobs right away, node %s", trace.Node)
		}

		p.mu.Lock()
		for _, plug := range p.plugs {
			plug.mu.Lock()

			var jobsToRun []*job
			for _, jobToRun := range plug.jobs {
				if !jobToRun.firstRun || time.Now().After(plug.wakeupTimes[jobToRun.interval]) {
					jobsToRun = append(jobsToRun, jobToRun)
				}
			}
			plug.mu.Unlock()
			p.mu.Unlock()

			for _, jobToRun := range jobsToRun {
				jobToRun.execute(jobToRun.ctx)
				jobToRun.firstRun = true
			}

			p.mu.Lock()
			plug.mu.Lock()
			for interv, wakeup := range plug.wakeupTimes {
				if time.Now().After(wakeup) {
					plug.wakeupTimes[interv] = wakeup.Add(interv)
				}
			}
			plug.mu.Unlock()
		}
		p.mu.Unlock()
	}
}

func (p *JobPlugin) Init(ctx context.Context) {
	p.jobs = make(map[string]*job)
	p.wakeupTimes = make(map[time.Duration]time.Time)
}

func (p *JobPlugin) Delegate(ctx context.Context, in adconnector.Body) (io.Reader, error) {
	jrunner.mu.Lock()
	if !jrunner.running {
		go jrunner.run(ctx)
		jrunner.running = true
	}
	jrunner.plugs[utils.GetTraceFromContext(ctx).Uuid+utils.GetTraceFromContext(ctx).Node] = p
	jrunner.mu.Unlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	logger := adlog.MustFromContext(ctx)
	inputMsg := &jobMessage{}
	err := adio.JsonToObjectCtx(ctx, bytes.NewReader(in), inputMsg)
	if err != nil {
		logger.Error("error deserializing", zap.Error(err))
		return nil, err
	}

	switch inputMsg.TaskType {
	case "TypeGetRunningJobs":
		return nil, nil
	case "TypeCancelJob":
		return nil, nil
	}

	// Set a wakeup time for the incoming interval if not set.
	jobInterval := time.Duration(inputMsg.Interval * inputMsg.IntervalUnit)

	if jobRate != 0 {
		jobInterval = jobRate
	}

	_, wok := p.wakeupTimes[jobInterval]
	if !wok {
		// Add min offset
		offset := time.Duration(rand.Int63n(int64(jobInterval)))
		if offset < time.Minute {
			offset = time.Minute
		}

		p.wakeupTimes[jobInterval] = time.Now().Add(offset)
		logger.Sugar().Infof("setting wakeup time of %s for %s", p.wakeupTimes[jobInterval].String(),
			jobInterval.String())
	}

	var task []byte

	// Look for the task in the cache
	taskCacheMu.Lock()
	sha := sha256.New()
	sha.Write(inputMsg.Task) // nolint
	chksum := sha.Sum(nil)
	var ok bool
	task, ok = taskCache[string(chksum)]
	if !ok {
		taskCache[string(chksum)] = inputMsg.Task
		task = taskCache[string(chksum)]
	} else {
		logger.Info("found task in cache")
	}
	taskCacheMu.Unlock()

	// Check if this request has a trace response
	rsp := utils.GetTraceResponse(ctx, inputMsg.TaskType, inputMsg.Task)

	jp, ok := p.Platform.Plugins[inputMsg.TaskType]
	if ok || rsp != nil {
		ej, ok := p.jobs[inputMsg.Name]
		if !ok {
			logger.Sugar().Debugf("starting job: %s", inputMsg.Name)
			j := &job{
				ctx:            ctx,
				name:           inputMsg.Name,
				interval:       jobInterval,
				task:           task,
				responseTopic:  inputMsg.ResponseTopic,
				cannedResponse: rsp,
				plugin:         jp,
			}

			p.jobs[inputMsg.Name] = j
			select {
			case jrunner.wakeup <- struct{}{}:
			default:
			}
		} else {
			logger.Sugar().Infof("job already exists: %s", inputMsg.Name)
			ej.interval = jobInterval
			if inputMsg.UpdateRunning {
				logger.Sugar().Infof("update running: %s", inputMsg.Name)

				ej.firstRun = false
				select {
				case jrunner.wakeup <- struct{}{}:
				default:
				}
			}
		}
		return nil, nil
	}
	return nil, fmt.Errorf("no plugin defined for job")
}

func (p *JobPlugin) RunJobNow(ctx context.Context, jobName string) bool {
	job, ok := p.jobs[jobName]
	if ok {
		job.firstRun = false
		select {
		case jrunner.wakeup <- struct{}{}:
		default:
		}
	}
	return ok
}
