package plugins

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adconnector"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adio"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adio/json"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adlog"
	"go.uber.org/zap"
)

type AsyncWorkConfig struct {
	ClassId    string
	ObjectType string
	QueueSize  int
	Timeout    int64
}

type ConnectorAsyncWorkUnit struct {
	ClassId    string
	ObjectType string
	Deadline   time.Time
	Key        string
	Plugin     string
	Request    []byte
}

type AsyncWorkResult struct {
	ClassId    string
	ObjectType string
	Error      interface{}
	Key        string
	Output     []byte
}

type AsyncWork struct {
	Platform *PluginManager
}

func (p *AsyncWork) Init(ctx context.Context) {
}

func (p *AsyncWork) Start(ctx context.Context) {
}

func (p *AsyncWork) Stop(ctx context.Context) {
}

func (p *AsyncWork) Delegate(ctx context.Context, in adconnector.Body) (io.Reader, error) {
	return nil, fmt.Errorf("not implemented")
}

// Start an asynchronous worker with stream channels as its i/o.
func (p *AsyncWork) StreamDelegate(ctx context.Context, inCh, outCh, errCh chan adconnector.Body,
	streamName string, in adconnector.Body) error {
	logger := adlog.MustFromContext(ctx)
	logger.Sugar().Infof("got stream input : %s", string(in))

	config := &AsyncWorkConfig{}
	err := json.UnmarshalWithCtx(ctx, in, config)
	if err != nil {
		return err
	}

	logger.Sugar().Infof("starting async worker %s", streamName)
	wkr := asyncWorkStream{
		Platform: p.Platform,
		config:   config,
		outputCh: outCh,
		errorCh:  errCh,
		inputCh:  inCh,
	}
	wkr.start(ctx)
	return nil
}

type asyncWorkStream struct {
	Platform *PluginManager
	// Configuration of this worker
	config *AsyncWorkConfig

	// Lock for map access
	mu sync.Mutex

	// Map of currently queued or running work, on receipt of work message if
	// it's request id matches a value in this map it will be dropped.
	// On completion of work Delegate() entries are removed.
	dedupQueue map[string]bool

	// Channel messages are queued into
	workQueue chan ConnectorAsyncWorkUnit

	// Stream output channel
	outputCh chan adconnector.Body
	// Stream error channel
	errorCh chan adconnector.Body
	// Stream input channel
	inputCh chan adconnector.Body
}

const (
	// The max size of the worker queue, if cloud requests larger it will be overwritten with this value.
	maxWorkQueueSize = 200
	// The default size for the worker queue, set if cloud does not set size in request.
	defaultWorkQueueSize = 50
)

// Start the async worker, initializing any variables and starting the work routines.
func (s *asyncWorkStream) start(ctx context.Context) {
	s.dedupQueue = make(map[string]bool)
	if s.config.QueueSize == 0 {
		s.config.QueueSize = defaultWorkQueueSize
	} else if s.config.QueueSize > maxWorkQueueSize {
		s.config.QueueSize = maxWorkQueueSize
	}

	s.workQueue = make(chan ConnectorAsyncWorkUnit, s.config.QueueSize)

	go s.worker(ctx)
	go s.inputHandler(ctx)
}

// Write the work result to the output channel and remove the key from dedup map.
func (s *asyncWorkStream) publishResult(ctx context.Context, res AsyncWorkResult) {
	s.mu.Lock()
	delete(s.dedupQueue, res.Key)
	s.mu.Unlock()

	// Construct work output response
	select {
	case s.outputCh <- adio.ObjectToJson(ctx, res, adio.JsonOptSerializeAllNoIndent):
	case <-ctx.Done():
	}
}

// Consume messages off the workQueue and execute plugins Delegate. Write result
// to stream output channel.
func (s *asyncWorkStream) worker(ctx context.Context) {
	logger := adlog.MustFromContext(ctx)
	for {
		select {
		case work := <-s.workQueue:
			wctx := ctx

			res := AsyncWorkResult{}
			res.Key = work.Key
			if !work.Deadline.IsZero() { // TODO: De-dup deadlines
				// Check if deadline has already passed and return error right away
				//
				// Current correct time
				curTime := time.Now()
				dur := work.Deadline.Sub(curTime)
				if dur <= 0 {
					// Deadline passed
					logger.Sugar().Infof("deadline %s has passed current time %s", work.Deadline.String(), curTime.String())
					res.Error = fmt.Errorf("%s deadline has passed", work.Deadline.String()).Error()
					s.publishResult(ctx, res)
					continue
				}
				// TODO: Add deadline to context
			}

			plug, ok := s.Platform.Plugins[work.Plugin]
			if !ok {
				res.Error = fmt.Errorf("%s plugin does not exist", work.Plugin).Error()
				s.publishResult(ctx, res)
				continue
			}
			rsp, err := plug.Delegate(wctx, work.Request)
			if err != nil {
				res.Error = err.Error()
			}
			if rsp != nil {
				buf := new(bytes.Buffer)
				buf.ReadFrom(rsp)
				res.Output = buf.Bytes()
			}
			s.publishResult(ctx, res)

		case <-ctx.Done():
			logger.Info("worker exited")
			// Stream has been closed, all clients will be notified of error by stream layer.
			return
		}
	}
}

// Handles input on the stream, deserializing the work unit messages and performing
// de-duplication against existing requests in the work queue. If requests key already
// exists in the queue logs a message and continues, otherwise publishes to the work channel.
func (s *asyncWorkStream) inputHandler(ctx context.Context) {
	logger := adlog.MustFromContext(ctx)
	for {
		select {
		case inputB := <-s.inputCh:
			input := ConnectorAsyncWorkUnit{}
			err := json.UnmarshalWithCtx(ctx, inputB, &input)
			if err != nil {
				// TODO: Publish error
				logger.Error("failed work input unmarshal", zap.Error(err))
				continue
			}
			s.mu.Lock()

			if s.dedupQueue[input.Key] {
				logger.Sugar().Infof("%s work already queued, dropping", input.Key)
				s.mu.Unlock()
				continue
			}
			s.dedupQueue[input.Key] = true
			s.mu.Unlock()

			select {
			case s.workQueue <- input:
			default:
				// If work queue is full return error to client now, do not block here.
				logger.Info("work queue is full")
				s.mu.Lock()
				delete(s.dedupQueue, input.Key)
				s.mu.Unlock()

				res := AsyncWorkResult{}
				res.Error = fmt.Errorf("device work queue is full")
				res.Key = input.Key
				s.publishResult(ctx, res)
			}
		case <-ctx.Done():
			return
		}
	}
}
