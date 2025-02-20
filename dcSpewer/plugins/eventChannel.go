package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math/rand"
	"os"
	"strconv"
	"sync"
	"time"

	utils "bitbucket-eng-sjc1.cisco.com/an/apollo-test/dcSpewer/utils"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adconnector"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adio"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adlog"
	"github.com/clbanning/mxj"
	"go.uber.org/zap"
)

var eventingEnabled bool
var eventingInterval time.Duration

func init() {
	eventIntRaw := os.Getenv("eventingInterval")
	if eventIntRaw != "" {
		eventIntervalSec, err := strconv.Atoi(eventIntRaw)
		if err == nil {
			eventingInterval = time.Second * time.Duration(eventIntervalSec)

		}
	}
	if eventingInterval == 0 {
		eventingInterval = 2 * time.Minute
	}

	if os.Getenv("eventingEnabled") != "" {
		eventingEnabled = true
	}
	geventer = &eventer{
		eventPlugins: make(map[string]*EventChannel),
	}
}

type eventChannelMsg struct {
	ClassFilters []string

	Control string

	Name string

	OutErrMsg string

	OutStatus int

	PropClassFilters map[string][]string

	ResetCounter bool

	SkipNotifyConsumerStart bool

	StartEvent int64

	Topic string
}

var geventer *eventer

// Global eventer, manages all connection event subscriptions
type eventer struct {
	mu      sync.Mutex
	started bool

	eventPlugins map[string]*EventChannel
}

func (e *eventer) run(ctx context.Context) {
	logger := adlog.MustFromContext(ctx)
	logger.Info("starting run")

	timer := time.NewTimer(eventingInterval)
	defer timer.Stop()

	for {
		einterval := eventingInterval + time.Duration(rand.Int63())%(eventingInterval/2)
		logger.Sugar().Infof("eventing in %s", einterval)
		timer.Reset(einterval)
		<-timer.C
		e.mu.Lock()

		if len(e.eventPlugins) == 0 {
			e.started = false
			e.mu.Unlock()
			return
		}

		for _, ep := range e.eventPlugins {
			ep.emitEvents()
		}

		e.mu.Unlock()
	}
}

func (e *eventer) addEventPlugin(ctx context.Context, ep *EventChannel) {
	e.mu.Lock()
	e.eventPlugins[utils.GetTraceFromContext(ctx).Uuid] = ep
	e.mu.Unlock()
}

func (e *eventer) removeEventPlugin(ctx context.Context, ep *EventChannel) {
	e.mu.Lock()
	delete(e.eventPlugins, utils.GetTraceFromContext(ctx).Uuid)
	e.mu.Unlock()
}

func (e *eventer) start(ctx context.Context) {
	e.mu.Lock()
	if !e.started {
		go e.run(ctx)
		e.started = true
	}
	e.mu.Unlock()
}

type EventChannel struct {
	// TODO : Other platforms shouldn't use event channel?
	xmlapi *XmlPlugin

	// Map of subsribers to their propclass filters
	subscribers map[string]map[string][]string
	// Map of subscribers to event counter
	counters map[string]int64
	mu       sync.Mutex

	// Cached context used to emit eventss
	ctx context.Context
}

func (p *EventChannel) Init(ctx context.Context) {
	p.subscribers = make(map[string]map[string][]string)
	p.counters = make(map[string]int64)
}

func (p *EventChannel) Delegate(ctx context.Context, in adconnector.Body) (io.Reader, error) {
	logger := adlog.MustFromContext(ctx)
	msg := &eventChannelMsg{}
	err := adio.JsonToObjectCtx(ctx, bytes.NewReader(in), msg)
	if err != nil {
		logger.Error("event message deserialize failure", zap.Error(err))
		return nil, err
	}

	if !eventingEnabled {
		return nil, nil
		/*
			msg.OutStatus = 2 // Failure
			msg.OutErrMsg = "eventing not enabled/supported"
			b := bytes.NewBuffer(nil)
			enc := json.NewEncoder(b)
			err = enc.Encode(msg)
			return b, err*/
	}

	logger.Info("got event subscription")
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ctx = ctx
	switch msg.Control {
	case "Start":
		if eventingEnabled {
			p.subscribers[msg.Topic] = msg.PropClassFilters
			go func() {
				geventer.addEventPlugin(ctx, p)
				geventer.start(ctx)
			}()
		}
	case "Stop":
		delete(p.subscribers, msg.Topic)
		if len(p.subscribers) == 0 {
			go geventer.removeEventPlugin(ctx, p)
		}
	}

	msg.OutStatus = 1 // Success
	b := bytes.NewBuffer(nil)
	enc := json.NewEncoder(b)
	err = enc.Encode(msg)
	return b, err
}

type eventOut struct {
	Counter int64
	Payload []byte
}

func (p *EventChannel) emitEvents() {
	logger := adlog.MustFromContext(p.ctx)

	p.mu.Lock()
	defer p.mu.Unlock()
	logger.Info("emitting events")
	var batch []byte
	var batchcount int
	for subscriber, subs := range p.subscribers {
		count := p.counters[subscriber]
		for class := range subs {
			mos := p.xmlapi.ResolveClass(p.ctx, class)
			for k, v := range mos {
				var molist []interface{}
				switch v := v.(type) {
				case []interface{}:
					molist = v
				case map[string]interface{}:
					molist = []interface{}{v}
				default:
					logger.Info("unknown mo type")
				}
				for _, moif := range molist {
					mo := moif.(map[string]interface{})
					if applyChange(p.ctx, k, mo) {
						xml, err := mxj.Map(mo).Xml(k)
						if err != nil {
							logger.Error("failed to marshal event", zap.Error(err))
							continue
						}
						batch = append(batch, xml...)
						batchcount++
						if batchcount > 100 {
							emitBatch(p.ctx, subscriber, count, batch)
							batch = batch[:0]
							batchcount = 0
							count++
						}
					}
				}
			}
		}
		if len(batch) > 0 {
			emitBatch(p.ctx, subscriber, count, batch)
			count++
		}
		p.counters[subscriber] = count
	}
}

func emitBatch(ctx context.Context, subscriber string, count int64, batch []byte) {
	if len(batch) == 0 {
		return
	}
	logger := adlog.MustFromContext(ctx)
	utils.DoingWork(ctx, utils.InventoryWork)
	defer utils.DoneWork(ctx, utils.InventoryWork)

	logger.Info("emitting event batch")
	eout := &eventOut{
		Payload: batch,
		Counter: count,
	}

	outbuf := new(bytes.Buffer)
	enc := json.NewEncoder(outbuf)
	err := enc.Encode(eout)
	if err != nil {
		logger.Error("failed to encode job message", zap.Error(err))
		return
	}

	eventMsg, err := utils.GetMessageReader(ctx, adconnector.Header{
		Type:      "TypeEvtChannelMsg",
		SessionId: subscriber,
	}, outbuf)
	if err != nil {
		logger.Error("failed to creat job message", zap.Error(err))
		return
	}
	getDispatcher(ctx).Dispatch(ctx, eventMsg)

}

// Apply a change to the mo.
// TODO: This needs to move to a generic approach so developers can describe changes spewer should emit
func applyChange(ctx context.Context, class string, mo map[string]interface{}) bool {
	logger := adlog.MustFromContext(ctx)
	switch class {
	case "faultInst":
		if rand.Intn(100) > 50 {
			return false // Faults changed randomly
		}
		mo["-lastTransition"] = time.Now().Format("2006-01-02T15:04:05.000") // UCSM format
		switch sevrand := rand.Intn(125); {
		case sevrand < 25:
			mo["-severity"] = "minor"
		case sevrand >= 25 && sevrand < 50:
			mo["-severity"] = "major"
		case sevrand >= 50 && sevrand < 75:
			mo["-severity"] = "cleared"
		case sevrand >= 75 && sevrand < 100:
			mo["-severity"] = "warning"
		default:
			mo["-severity"] = "info"
		}
		logger.Sugar().Debugf("messed with severity. new: %s", mo["-severity"])
		return true
	}
	// TODO: Commit changes to xml plug
	return false
}
