package emulatorConnection

import (
	"bytes"
	"compress/flate"
	"context"
	"io"
	"sync"
	"time"

	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adconnector"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adio"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adlog"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

type flowMessage struct {
	ObjectType string

	Bytes int `bson:"Bytes"`

	Messages int `bson:"Messages"`

	Priority int `bson:"Priority"`
}

type flowAck struct {
	ObjectType string

	QueueSize int `bson:"QueueSize"`
}

// Routine writes messages to the Intersight service
func (ce *connectionEmulator) writeLoop(ctx context.Context, wg *sync.WaitGroup) {
	logger := adlog.MustFromContext(ctx)
	defer logger.Info("write exit")
	defer wg.Done()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	defer func() {
		if cErr := ce.ws.Close(); cErr != nil {
			logger.Error("ws close fail", zap.Error(cErr))
		}
		ce.cancel()
	}()

	werr := ce.ws.WriteMessage(websocket.PingMessage, []byte(""))
	if werr != nil {
		logger.Error("write error", zap.Error(werr))
		return
	}

	for {
		flow := &flowMessage{}

		// Record data sent in current window, the window is defined as the period between receipt of
		// a flow control message from Intersight.

		// Total messages sent this window.
		msgsSentThisWindow := 0

		// Total bytes sent this window.
		bytesSentThisWindow := 0

		startFlowWait := time.Now()
	flowOut:
		for {
			if !ce.flowControlEnabled || apiVersion < 3 {
				break
			}
			select {
			case flow = <-ce.flowChannel:
				logger.Sugar().Infof("Got flow control: %d / %d", flow.Messages, flow.Bytes)

				// Write ack of flow control message
				ackMsg := &flowAck{}

				// Send the current queue size to the cloud. As Intersight backpressures the device the queue size will grow.
				//ackMsg.SetQueueSize(int(GetCloudQueueSize()))

				flowAck, err := adconnector.NewMessage(ctx, adconnector.Header{Type: "FlowControl"},
					adio.ObjectToJson(ctx, ackMsg, adio.JsonOptSerializeAllNoIndent))
				if err != nil {
					// Should never happen
					logger.Fatal("failed to create flow control ack")
				}
				// Compress the message
				flowFlateMu.Lock()
				buf := new(bytes.Buffer)
				flowFlateWriter.Reset(buf)
				_, err = flowFlateWriter.Write(flowAck)
				if err != nil {
					logger.Error("Failed to compress buffer", zap.Error(err))
					flowFlateMu.Unlock()
					return
				}
				err = flowFlateWriter.Close()
				if err != nil {
					logger.Error("Failed to close and flush compression writer", zap.Error(err))
					flowFlateMu.Unlock()
					return
				}
				flowFlateMu.Unlock()
				err = ce.ws.WriteMessage(websocket.TextMessage, buf.Bytes())
				if err != nil {
					logger.Error("Failed to write flow control ack", zap.Error(err))
				}
				// Record the flow control ack as a message.
				msgsSentThisWindow++
				bytesSentThisWindow += buf.Len()
				buf.Reset()
				break flowOut
				// Continue sending pings when backpressured
			case <-ticker.C:
				// Limit the amount of time we'll wait for a flow message
				// This case is not expected, it is a bug if the cloud does not send a flow message in this window.
				if time.Since(startFlowWait) > time.Hour {
					logger.Error("Haven't received a flow control message within timeout, closing connection")
					return
				}
				// Only send the ping if we're not currently waiting on a ping response
				werr := ce.ws.WriteMessage(websocket.PingMessage, []byte(""))
				if werr != nil {
					logger.Error("write error", zap.Error(werr))
					return
				}
				/*select {
				case pingRt <- struct{}{}:
					pingSent = time.Now()
					if err := d.ws.WriteMessage(websocket.PingMessage, []byte(pingSent.String())); err != nil {
						logger.Error("error sending message to astro on websocket", zap.Error(err))
						return
					}
				default:
				}*/
			case <-ctx.Done():
				return
			}
		}

		// Send messages until we hit the window limit
		for (!ce.flowControlEnabled || apiVersion < 3) || (msgsSentThisWindow < flow.Messages && bytesSentThisWindow < flow.Bytes) {

			select {
			case msg := <-ce.writeCh:
				n, werr := ce.writeMessage(msg)
				if werr != nil {
					logger.Error("write error", zap.Error(werr))
					return
				}

				msgsSentThisWindow++
				bytesSentThisWindow += int(n)
			case <-ticker.C:
				// Only send the ping if we're not currently waiting on a ping response
				/*select {
				case pingRt <- struct{}{}:
					pingSent = time.Now()
					if err := d.ws.WriteMessage(websocket.PingMessage, []byte(pingSent.String())); err != nil {
						logger.Error("error sending message to astro on websocket", zap.Error(err))
						return
					}
				default:
				}*/
				werr := ce.ws.WriteMessage(websocket.PingMessage, []byte(""))
				if werr != nil {
					logger.Error("write error", zap.Error(werr))
					return
				}
			case <-ctx.Done():
				logger.Info("exiting on quit")
				return
			}
		}

		logger.Sugar().Infof("hit send limit, waiting on next flow control message. Sent %d messages, %d bytes",
			msgsSentThisWindow, bytesSentThisWindow)
	}

}

func (ce *connectionEmulator) writeMessage(msg *writeMesage) (int64, error) {
	defer close(msg.done)
	wr, werr := ce.ws.NextWriter(websocket.TextMessage)
	if werr != nil {
		return 0, werr
	}

	n, werr := io.Copy(wr, msg.buf)
	if werr != nil {
		return n, werr
	}

	werr = wr.Close()
	if werr != nil {
		return n, werr
	}
	return n, nil
}

var flateWriter *flate.Writer
var flateMu sync.Mutex
var flowFlateWriter *flate.Writer
var flowFlateMu sync.Mutex

func init() {
	flateWriter, _ = flate.NewWriter(nil, 1)     // nolint
	flowFlateWriter, _ = flate.NewWriter(nil, 1) // nolint
}

type writeMesage struct {
	buf  io.Reader
	done chan struct{}
}

// Sends a message to the write channel for writing to the websocket
func (ce *connectionEmulator) Dispatch(ctx context.Context, in io.Reader) {
	logger := adlog.MustFromContext(ce.ctx)
	flateMu.Lock()
	defer flateMu.Unlock()
	buf := new(bytes.Buffer)

	if apiVersion >= 2 {
		flateWriter.Reset(buf)

		_, flateerr := io.Copy(flateWriter, in)
		if flateerr != nil {
			logger.Error("Failed to write to flate", zap.Error(flateerr))
			return
		}
		err := flateWriter.Close()
		if err != nil {
			logger.Error("Failed to close flate writer", zap.Error(err))
			return
		}
	} else {
		_, buferr := io.Copy(buf, in)
		if buferr != nil {
			logger.Error("Failed to write to buffer", zap.Error(buferr))
			return
		}
	}

	writeMesage := &writeMesage{buf: buf, done: make(chan struct{})}

	select {
	case ce.writeCh <- writeMesage:
	case <-ctx.Done():
	}

	select {
	case <-writeMesage.done:
	case <-ctx.Done():
	}
}
