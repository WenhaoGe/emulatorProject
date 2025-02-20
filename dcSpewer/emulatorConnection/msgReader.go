package emulatorConnection

import (
	"bytes"
	"compress/flate"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"bitbucket-eng-sjc1.cisco.com/an/apollo-test/dcSpewer/utils"
	adapp_stim_base "bitbucket-eng-sjc1.cisco.com/an/barcelona/adapp/stim/base"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adconnector"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adio"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adlog"
	"go.uber.org/zap"
)

// Implements io.Writer to read messages off of the websocket reader
type wsMsgWriter struct {
	header adconnector.Header // Message header
	msg    adconnector.Body   // Message body
	ctx    context.Context    // Request context, used to cancel and set a deadline on the message processing.
}

// Not used, io.Copy will use ReadFrom if exists instead of Write. Implemented to
// satisfy the io.Writer interface
func (wr *wsMsgWriter) Write(p []byte) (int, error) {
	return 0, errors.New("Not Implemented. Use ReadFrom()")
}

// Read the adconnector.Message off of the passed in reader.
// First reads the json encoded message.Header to determine the message type
// In case of device connector upgrade message copy the reader contents directly
// to persist location defined in platform config
// In case of any other message types the message body is read in full into a byte
// buffer with the io.ReadAll message
// Returns number of bytes read from reader and any error encountered
func (wr *wsMsgWriter) ReadFrom(r io.Reader) (n int64, err error) {
	h, n, err := adconnector.ReadHeader(r)

	if err != nil {
		return n, err
	}

	wr.header = *h

	body, bs, err := adconnector.ReadBody(r, *h)
	if err != nil {
		return int64(bs) + n, err
	}

	wr.msg = body
	return int64(len(wr.msg)) + n, err
}

// Reads messages off the websocket and delegates them to the appropriate plugin
func (ce *connectionEmulator) readLoop(ctx context.Context) {
	logger := adlog.MustFromContext(ctx)
	handler := make(chan struct{}, 100)

	for {
		_, reader, rerr := ce.ws.NextReader()
		if rerr != nil {
			logger.Error("read error", zap.Error(rerr))
			return
		}

		err := ce.handleMessage(ctx, reader, handler)
		if err != nil {
			logger.Error("error handling message", zap.Error(err))
			return
		}
	}
}

func (ce *connectionEmulator) handleMessage(ctx context.Context, r io.Reader, handler chan struct{}) error {
	logger := adlog.MustFromContext(ctx)
	select {
	case readLimiter <- struct{}{}:
	case <-ctx.Done():
		return fmt.Errorf("context cancelled")
	}
	defer func() {
		<-readLimiter
	}()

	d := &wsMsgWriter{
		ctx: ctx,
	}

	var cerr error
	if apiVersion >= 3 && ce.readCompressionEnabled {
		compressedReader := flate.NewReader(r)
		_, cerr = io.Copy(d, compressedReader) // #nosec G110
		// Always close compressed reader
		compressClose := compressedReader.Close()
		if compressClose != nil {
			logger.Error("failed to close compressed reader", zap.Error(compressClose))
		}
	} else {
		_, cerr = io.Copy(d, r)
	}

	if cerr != nil {
		logger.Error("read error", zap.Error(cerr))
		return cerr
	}

	if d.header.Type == "FlowControl" {
		logger.Info("got flow")

		if !ce.flowControlEnabled {
			logger.Warn("received flow control message when flow control is disabled")
			return nil
		}
		flowMsg := &flowMessage{}
		err := adio.JsonToObjectCtx(ctx, bytes.NewReader(d.msg), flowMsg)
		if err != nil {
			logger.Error("failed unmarshal of flow control message", zap.Error(err))
			return err
		}
		select {
		case ce.flowChannel <- flowMsg:
		case <-ctx.Done():
		}
		return nil
	}

	select {
	case handler <- struct{}{}:
	case <-ctx.Done():
		return fmt.Errorf("context cancelled")
	case <-time.After(10 * time.Second):
		logger.Error("timeout getting handler lock")
	}
	go func() {
		defer func() {
			<-handler
		}()

		// Special hanndling for rest stim responses
		if d.header.Type == "TypeRestStim" {
			var respStim adapp_stim_base.RestStim

			err := adio.JsonToObjectCtx(ctx, bytes.NewReader(d.msg), &respStim)
			if err != nil {
				// This could happen in normal case when the websocket connection is closed.
				logger.Error("failed to read response rest stim", zap.Error(err))
				return
			}

			ce.mu.Lock()
			rspChan, ok := ce.rspMap[respStim.RequestId]
			ce.mu.Unlock()
			if ok {
				select {
				case rspChan <- d.msg:
				default:
					logger.Info("dropping response")
				}
			}
			return
		}

		if d.header.RequestId != "" {
			ce.mu.Lock()
			rspChan, ok := ce.rspMap[d.header.RequestId]
			ce.mu.Unlock()
			if ok {
				select {
				case rspChan <- d.msg:
				default:
					logger.Info("dropping response")
				}
			}
		}

		if d.header.Type == "TypeControl" {
			go ce.handleControlMessage(d.msg)
		} else {
			switch d.header.Type {
			case "TypeEvtChannelCtrlMsg":
				fallthrough
			case "TypeJobStim":
				utils.DoingWork(ctx, utils.ControlWork)
				defer utils.DoneWork(ctx, utils.ControlWork)
			default:
				utils.DoingWork(ctx, utils.InventoryWork)
				defer utils.DoneWork(ctx, utils.InventoryWork)
			}

			rsp, err := ce.platform.Delegate(ctx, d.header.Type, d.msg)
			if d.header.RequestId != "" {
				if err != nil {
					d.header.Type = adconnector.TypeError
					rsp = strings.NewReader(err.Error())
				} else if rsp == nil {
					rsp = strings.NewReader("")
				}
				rspMsg, err := utils.GetMessageReader(ctx, d.header, rsp)
				if err != nil {
					logger.Error("can't construct response", zap.Error(err))
					return
				}
				ce.Dispatch(ctx, rspMsg)
			}
		}

	}()
	return nil
}
