package plugins

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"os"
	"strconv"

	"io"

	"bitbucket-eng-sjc1.cisco.com/an/apollo-test/dcSpewer/utils"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adconnector"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adio"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adlog"
	"github.com/golang/groupcache/lru"
	"go.uber.org/zap"
)

var intervalOverride time.Duration

func init() {
	streamIntervalRaw := os.Getenv("STREAM_INTERVAL")

	if streamIntervalRaw != "" {
		spawnRateSec, err := strconv.Atoi(streamIntervalRaw)
		if err != nil {
			panic(err)
		}

		intervalOverride = time.Second * time.Duration(spawnRateSec)
	}

	gstreamer = &streamer{
		outCh: make(chan *streamOutput),
	}

	// Disabling message cache for now to reduce memory
	if os.Getenv("ENABLE_STREAM_CACHE") != "" {
		gstreamer.msgCache = lru.New(5000)
	}

}

var gstreamer *streamer

// Global streamer managing all stream instances
type streamer struct {
	mu      sync.Mutex
	started bool

	outCh    chan *streamOutput
	msgCache *lru.Cache
}

func (p *streamer) start(ctx context.Context) {
	p.mu.Lock()
	if !p.started {
		go p.run(ctx)
		p.started = true
	}
	p.mu.Unlock()
}

func (p *streamer) run(ctx context.Context) {
	for output := range p.outCh {
		msgHdr, err := output.msg.GetHeader()
		if err != nil {
			continue
		}
		p.mu.Lock()
		if p.msgCache != nil {
			p.msgCache.Add(cacheKey{
				deviceId:       utils.GetTraceFromContext(output.ctx).Uuid,
				streamId:       output.streamId,
				sequenceNumber: msgHdr.SequenceNumber}, output.msg)
		}
		p.mu.Unlock()

		getDispatcher(output.ctx).Dispatch(output.ctx, bytes.NewReader(output.msg))
	}
}

func (p *streamer) getFromCache(ctx context.Context, key cacheKey) (interface{}, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.msgCache == nil {
		return nil, false
	}
	return p.msgCache.Get(key)
}

type StreamPlugin struct {
	Platform *PluginManager

	streams map[string]*stream

	streamOps   map[string]*streamOperation
	streamTypes map[string]*stream

	// Lock for access to above map
	streamMapLock sync.RWMutex
}

type cacheKey struct {
	deviceId       string
	streamId       string // The stream id
	sequenceNumber int    // The sequence number of the message
}

type streamMsg struct {
	StreamName string
	ObjectType string

	// Start parameters
	Input         []byte
	PluginName    string
	PollInterval  int64
	BatchSize     int
	ResponseTopic string

	// Input

	// Fetch
	Sequences []int

	// Close

}

type stream struct {
	cancel        context.CancelFunc
	name          string
	pluginName    string
	interval      time.Duration
	batchSize     int
	plugin        Plugin
	input         []byte
	responsetopic string

	outCh   chan *streamOutput
	inputCh chan *streamMsg

	seq          int
	streamplugin *StreamPlugin
}

// TODO: Support tuning stream behavior including:
// TODO: 1) Increase rate of sending.
// TODO: 2) Introduce message drops
// TODO: 3) Duplicate sending
// TODO: 4) Stream breaking error publishing
func (p *StreamPlugin) Init(ctx context.Context) {
	p.streams = make(map[string]*stream)
	p.streamOps = make(map[string]*streamOperation)
	p.streamTypes = make(map[string]*stream)
}

func (p *StreamPlugin) Delegate(ctx context.Context, in adconnector.Body) (io.Reader, error) {
	logger := adlog.MustFromContext(ctx)

	msg := &streamMsg{}
	err := adio.JsonToObjectCtx(ctx, bytes.NewReader(in), msg)
	if err != nil {
		logger.Error("error deserializing", zap.Error(err))
		return nil, err
	}

	gstreamer.start(ctx)

	switch msg.ObjectType {
	case "connector.StartStream":
		//logger.Sugar().Infof("starting stream %+v", msg)
		plugin, ok := p.Platform.Plugins[msg.PluginName]
		if !ok {
			return nil, fmt.Errorf("plugin %s not found", msg.PluginName)
		}
		sctx, cancel := context.WithCancel(ctx)
		s := &stream{
			cancel:        cancel,
			name:          msg.StreamName,
			pluginName:    msg.PluginName,
			plugin:        plugin,
			outCh:         gstreamer.outCh,
			interval:      time.Duration(msg.PollInterval),
			input:         msg.Input,
			responsetopic: msg.ResponseTopic,
			inputCh:       make(chan *streamMsg),
			batchSize:     msg.BatchSize,
			streamplugin:  p,
		}

		// Override interval if set
		if intervalOverride != 0 {
			s.interval = intervalOverride
		}

		streamType := strings.Split(strings.Split(msg.StreamName, ":")[0], ",")[0]
		logger.Sugar().Infof("starting stream : %s", msg.StreamName)
		p.streamMapLock.Lock()
		es, ok := p.streams[msg.StreamName]
		p.streamMapLock.Unlock()
		if ok {
			logger.Info("sstream exisst")
			p.streamMapLock.RLock()
			if streamOp, ok := p.streamOps[streamType]; ok {
				select {
				case streamOp.streamNotif <- es:
				default:
				}
			}
			p.streamMapLock.RUnlock()
			return bytes.NewBuffer(adio.ObjectToJson(ctx, es.seq, adio.JsonOptSerializeAllNoIndent)), nil
		}

		p.streamMapLock.Lock()
		defer p.streamMapLock.Unlock()
		etype, ok := p.streamTypes[streamType]
		if ok {
			logger.Info("cancelling previous stream type")
			etype.cancel()
		}

		if streamOp, ok := p.streamOps[streamType]; ok {
			select {
			case streamOp.streamNotif <- s:
			default:
			}
		}

		p.streams[msg.StreamName] = s
		p.streamTypes[streamType] = s
		go s.run(sctx)
		return bytes.NewBuffer(adio.ObjectToJson(ctx, 0, adio.JsonOptSerializeAllNoIndent)), nil
	case "connector.CloseStreamMessage":
		p.streamMapLock.Lock()
		if s, ok := p.streams[msg.StreamName]; ok {
			s.cancel()
		}
		p.streamMapLock.Unlock()
	case "connector.FetchStreamMessage":
		if os.Getenv("ENABLE_STREAM_CACHE") == "" {
			return nil, fmt.Errorf("not in cache")
		}

		logger.Sugar().Infof("fetching sequences: %v", msg.Sequences)

		for _, s := range msg.Sequences {
			msg, ok := gstreamer.getFromCache(ctx, cacheKey{
				deviceId:       utils.GetTraceFromContext(ctx).Uuid,
				streamId:       msg.StreamName,
				sequenceNumber: s})
			if !ok {
				fmt.Println("cache miss")
				return nil, fmt.Errorf("not in cache")
			}
			getDispatcher(ctx).Dispatch(ctx, bytes.NewReader(msg.(adconnector.Message)))
		}
	case "connector.StreamInput":
		p.streamMapLock.Lock()
		s, ok := p.streams[msg.StreamName]
		p.streamMapLock.Unlock()
		if ok {
			s.inputCh <- msg
		}
	default:
		logger.Sugar().Infof("unsupported stream %+v", msg)
		return nil, fmt.Errorf("unsupported stream message")
	}

	return nil, nil
}

func (p *StreamPlugin) StartOperation(ctx context.Context, in map[string]interface{}) (Operation, error) {
	logger := adlog.MustFromContext(ctx)

	logger.Sugar().Infof("received stream op start %+v", in)

	name, ok := in["StreamName"].(string)
	if !ok {
		return nil, fmt.Errorf("input not of type stream start")
	}
	topic, ok := in["Topic"].(string)
	if !ok {
		return nil, fmt.Errorf("input not of type stream start")
	}

	logger.Sugar().Infof("got name for stream start : %s", name)

	streamOp := &streamOperation{
		streamPlugin: p,
		streamNotif:  make(chan interface{}, 1),
	}
	//msg.SetMemberId(base.GetNodeId())

	p.streamMapLock.Lock()
	p.streamOps[name] = streamOp
	p.streamMapLock.Unlock()

	// Always remove from plugin ops map
	defer func() {
		p.streamMapLock.Lock()
		delete(p.streamOps, name)
		p.streamMapLock.Unlock()
	}()
	// Send request to cloud to start the stream
	startMsg, err := adconnector.NewMessage(ctx, adconnector.Header{SequenceNumber: -1,
		SessionId: topic, Type: "TypeStreamer"},
		adio.ObjectToJson(ctx, in, adio.JsonOptSerializeAllNoIndent))
	if err != nil {
		return nil, err
	}

	getDispatcher(ctx).Dispatch(ctx, bytes.NewReader(startMsg))

	// Wait for the stream to be started
	waitCtx, cancel := context.WithDeadline(ctx, time.Now().Add(time.Minute))
	defer cancel()

	select {
	case out := <-streamOp.streamNotif:
		switch out := out.(type) {
		case *stream:
			logger.Info("stream opened successfully")
			streamOp.stream = out
		case error:
			return nil, out
		default:
			return nil, fmt.Errorf("unexpected output from stream start")
		}
	case <-waitCtx.Done():
		logger.Info("stream open time out")
		return nil, waitCtx.Err()
	}

	return streamOp, nil //fmt.Errorf("not implemented")
}

// Wrapper around a stream, started from the operations layer
type streamOperation struct {
	// Handle to the stream plugin.s
	streamPlugin *StreamPlugin

	// Channel stream plugin will send to when stream has been started.
	// Message will contain either an error if stream failed to start or an error.
	streamNotif chan interface{}

	// The stream started by this operation.
	stream *stream
}

// Cancel the stream associated with this operation.
func (s *streamOperation) Stop(ctx context.Context) error {
	if s.stream != nil {
		s.stream.cancel()
	}
	return nil
}

// Check if stream is still active and is currently running within the stream plugin.
func (s *streamOperation) Active(ctx context.Context) bool {
	if s.stream == nil {
		return false
	}

	s.streamPlugin.streamMapLock.RLock()
	_, ok := s.streamPlugin.streams[s.stream.name]
	s.streamPlugin.streamMapLock.RUnlock()
	if !ok {
		s.stream = nil
	}
	return ok
}

// Output from a stream
type streamOutput struct {
	ctx      context.Context
	streamId string              // The source stream id
	msg      adconnector.Message // The message emitted from the stream
}

type Batcher interface {
	BatchDelegate(ctx context.Context, in adconnector.Body) ([]map[string]interface{}, error)
}

type Streamer interface {
	StreamDelegate(ctx context.Context, inCh, outCh, errCh chan adconnector.Body,
		streamName string, in adconnector.Body) error
}

func (s *stream) run(ctx context.Context) {
	logger := adlog.MustFromContext(ctx)

	defer func() {
		logger.Sugar().Infof("%s stream ended", s.name)
		s.streamplugin.streamMapLock.Lock()
		delete(s.streamplugin.streams, s.name)
		s.streamplugin.streamMapLock.Unlock()
	}()
	batcher, ok := s.plugin.(Batcher)
	if ok {
		logger.Info("starting batch collect", zap.Int(adlog.LogGenericNumber, s.batchSize))
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()

		var outBuf []map[string]interface{}
		batchSize := 0
		for {
			select {
			case <-ticker.C:
				//logger.Info("running batchdelegate")
				out, err := batcher.BatchDelegate(ctx, s.input)
				if err != nil {
					fmt.Println("delegate err: ", err.Error())
				} else {
					batchSize++
					outBuf = append(outBuf, out...)
					if batchSize >= s.batchSize {
						if len(outBuf) == 0 {
							batchSize = 0
							continue
						}
						logger.Sugar().Debugf("emitting stream message : %s", string(adio.ObjectToJson(ctx, outBuf, nil)))
						streamMsg, err := adconnector.NewMessage(ctx, adconnector.Header{
							Type:           s.pluginName,
							SessionId:      s.responsetopic,
							SequenceNumber: s.seq,
						}, adio.ObjectToJson(ctx, outBuf, adio.JsonOptSerializeAllNoIndent))
						if err != nil {
							return
						}
						s.outCh <- &streamOutput{ctx: ctx, streamId: s.name, msg: streamMsg}

						outBuf = nil
						s.seq++
						batchSize = 0
					}

				}
			case <-ctx.Done():
				return
			}
		}
	}

	streamer, ok := s.plugin.(Streamer)
	if ok {
		inch := make(chan adconnector.Body)
		outch := make(chan adconnector.Body)
		errch := make(chan adconnector.Body)

		err := streamer.StreamDelegate(ctx, inch, outch, errch, "", s.input)
		if err != nil {
			logger.Error("failed to start stream plugin", zap.Error(err))
			//TODO: Return error
			return
		}

		go func() {
			defer func() {
				var errMsg adconnector.Body
				select {
				case errMsg = <-errch:
				default:
				}

				outMsg, err := adconnector.NewMessage(ctx, adconnector.Header{SequenceNumber: -1,
					SessionId: s.responsetopic, Type: "TypeStreamer"}, errMsg)
				if err != nil {
					logger.Error("Failed to create output stream message", zap.Error(err))
					return
				}
				s.outCh <- &streamOutput{ctx: ctx, streamId: s.name, msg: outMsg}
			}()

			for {
				select {
				case output, ok := <-outch:
					if !ok {
						return
					}
					streamMsg, err := adconnector.NewMessage(ctx, adconnector.Header{
						Type:           s.pluginName,
						SessionId:      s.responsetopic,
						SequenceNumber: s.seq,
					}, output)
					if err != nil {
						return
					}
					s.outCh <- &streamOutput{ctx: ctx, streamId: s.name, msg: streamMsg}

					s.seq++
				case <-ctx.Done():
					return
				}

			}
		}()

		for {
			select {
			case input, ok := <-s.inputCh:
				if !ok {
					return
				}

				select {
				case inch <- input.Input:
				case <-ctx.Done():
				}

			case <-ctx.Done():
				return
			}
		}
	}

	logger.Info("nothing to do unsupported stream")

}
