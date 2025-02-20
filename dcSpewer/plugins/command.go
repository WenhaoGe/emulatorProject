package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adconnector"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adio"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adlog"
	"github.com/kr/pty"
	"github.com/mattn/go-shellwords"
	"go.uber.org/zap"
)

type Command struct {
}

// Request to resize the terminal size of the pseudo terminal
type resizeTerminal struct {
	Rows    int
	Columns int
}

// A remote terminal session
type session struct {
	pty       *os.File  // The pseudo terminal fd
	cmd       *exec.Cmd // The command being run
	sessionId string    // Unique Id of the session used for routing messages
	ctx       context.Context
	input     chan adconnector.Body
	output    chan adconnector.Body
	err       chan adconnector.Body
	quit      chan struct{}

	// Wait group to ensure all routines have exited before closing command
	wg sync.WaitGroup

	// Pipes attached to command output for non-terminal command sessions
	stderr io.ReadCloser
	stdout io.ReadCloser

	// The timeout for command to exit or receive input from the cloud service
	timeout time.Duration
}

// Timeout for writing into a command session
const commandInputDeadline = 10 * time.Minute

func (p *Command) Init(ctx context.Context) {
	adlog.MustFromContext(ctx).Info("plugin Command Init() called")
}

// Send a Close message to the remote service to notify of a session close
func (s *session) sendClose() {
	select {
	case _, ok := <-s.quit:
		if !ok {
			return
		}
	default:
		close(s.quit)
	}
	s.wg.Wait()
	close(s.output)
}

// Max size of a read off the pseudo terminal
const BUF_SIZE = 1024

// Read output from the pseudo terminal attached to the subprocess stdout/stderr.
// Buffers output from the command before sending to the stream output.
func (s *session) readPty() {
	logger := adlog.MustFromContext(s.ctx)

	bufCh := make(chan []byte, 1)
	buf := make([]byte, BUF_SIZE)
	defer func() {
		cErr := s.pty.Close()
		if cErr != nil {
			logger.Debug("Failed to close pty", zap.Error(cErr))
		}
	}()

	defer s.sendClose()

	defer s.wg.Done()

	// Buffer reads off the terminal
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		var allBuf []byte
		for {
			select {
			case <-s.quit:
				// Drain the read buffer before closing the command stream
				for len(bufCh) > 0 {
					select {
					case in := <-bufCh:
						allBuf = append(allBuf, in...)
					default:
					}
				}
				if len(allBuf) > 0 {
					cmd := &terminalmsg{}
					cmd.Stream = allBuf
					select {
					case s.output <- adio.ObjectToJson(s.ctx, cmd, adio.JsonOptSerializeAllNoIndent):
					case <-s.ctx.Done():
						return
					}
				}
				return
			case in := <-bufCh:
				allBuf = append(allBuf, in...)
				// If the limit has been reached send immediately
				if len(allBuf) < BUF_SIZE {
					continue
				}
				// Send every 20 milliseconds if there is anything in the buffer
			case <-time.After(20 * time.Millisecond):
				if len(allBuf) == 0 {
					continue
				}
			}
			cmd := &terminalmsg{}
			cmd.Stream = allBuf
			select {
			case s.output <- adio.ObjectToJson(s.ctx, cmd, adio.JsonOptSerializeAllNoIndent):
			case <-s.ctx.Done():
				return
			}
			allBuf = allBuf[:0]
		}
	}()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.quit:
			return
		default:
		}
		n, err := s.pty.Read(buf)
		if err != nil {
			logger.Info("Error reading from cmd", zap.Error(err))
			return
		}
		// Create a copy of read byte slice and send to buffering routine
		sendBuf := make([]byte, n)
		copy(sendBuf, buf[:n])
		select {
		case bufCh <- sendBuf:
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *session) runTerminal() {
	logger := adlog.MustFromContext(s.ctx)

	var err error
	defer func() {
		defer s.sendClose()
		logger.Debug("Closing command")
		// Close the pseudo terminal fd
		err = s.pty.Close()
		if err != nil {
			logger.Debug("Failed to close file", zap.Error(err))
		}
		// Command may be nil in case of file write
		if s.cmd != nil {
			// Send a sigterm to the forked process to gracefully exit
			err = s.cmd.Process.Signal(syscall.SIGTERM)
			// If process hasn't exited after 5 seconds. Send a SIGKILL
			// to handle the case that the process is ignoring SIGTERM or is not in a
			// good state to gracefully exit
			killTimer := time.AfterFunc(5*time.Second, func() {
				killErr := s.cmd.Process.Kill()
				if killErr != nil {
					logger.Fatal("Failed to kill command process", zap.Error(killErr))
				}
			})

			if err != nil {
				logger.Error("Failed to send SIGTERM to command process", zap.Error(err))
			} else {
				// Wait on the process to exit
				err = s.cmd.Wait()
				if err != nil {
					logger.Error("Command exited with error", zap.Error(err))
				}
				killTimer.Stop()
			}
		}
	}()

	defer s.wg.Done()

	for {
		select {
		case rawmsg, ok := <-s.input:
			if !ok {
				logger.Debug("Stream closed")
				return
			}
			msg := &terminalmsg{}
			err = adio.JsonToObjectCtx(s.ctx, bytes.NewReader(rawmsg), msg)
			if err != nil {
				logger.Error("Failed to deserialize command input", zap.Error(err))
				continue
			}

			switch msg.MsgType {
			case "0":
				logger.Error("Init message received after init")
			case "1":
				logger.Debug("Writing input msg")
				var n int
				// Security audit WARNING: This is user input to a pseudo terminal
				n, err = s.pty.Write(msg.Stream)
				// On any input error close the session
				if err != nil {
					logger.Error("Error writing to fd", zap.Error(err))
					s.sendClose()
					return
				}
				if n != len(msg.Stream) {
					logger.Error("Write did not complete")
					s.sendClose()
					return
				}

			case "2":
				var args resizeTerminal
				err = json.Unmarshal([]byte(msg.Stream), &args)

				if err != nil {
					// Keep going. A resize error is recoverable
					logger.Error("Malformed remote command")
					break
				}
				logger.Sugar().Debugf("Setting size to: %d %d", uint16(args.Rows), uint16(args.Columns))

				window := struct {
					row uint16
					col uint16
					x   uint16
					y   uint16
				}{
					uint16(args.Rows),
					uint16(args.Columns),
					0,
					0,
				}
				// Retrieve terminal dimensions with system call
				// It is flagged by go vet as a potential security issue, but this is safe.
				_, _, errno := syscall.Syscall(
					syscall.SYS_IOCTL,
					s.pty.Fd(),
					syscall.TIOCSWINSZ,
					uintptr(unsafe.Pointer(&window)),
				) // #nosec
				if errno != 0 {
					return
				}

			default:
				logger.Sugar().Errorf("Unknown msg type: %s", msg.MsgType)
			}

		case <-time.After(commandInputDeadline):
			s.sendError(s.ctx, -1, errors.New("Command terminal input timeout"))
			logger.Sugar().Infof("Session %s i/o timeout", s.sessionId)
			return
		case <-s.quit:
			return
		case <-s.ctx.Done():
			return
		}
	}
}

func (p *Command) Delegate(ctx context.Context, in adconnector.Body) (io.Reader, error) {
	// No-op
	return nil, nil
}

type exitMsg struct {
	ExitCode int
	Error    string
}

// Report exit code status and error to the stream.
func (s *session) sendError(ctx context.Context, exitCode int, err error) {
	res := &exitMsg{
		ExitCode: exitCode,
	}
	if err != nil {
		res.Error = err.Error()
	}
	select {
	case s.err <- adio.ObjectToJson(s.ctx,
		res,
		adio.JsonOptSerializeAllNoIndent):
	default:
	}
}

// Start a command subprocess attaching pipes to output/input of the process.
func (s *session) startCommand() error {
	logger := adlog.MustFromContext(s.ctx)

	var err error

	s.stdout, err = s.cmd.StdoutPipe()
	if err != nil {
		logger.Error("Failed to attach stdout", zap.Error(err))
		return err
	}

	s.stderr, err = s.cmd.StderrPipe()
	if err != nil {
		logger.Error("Failed to attach stderr", zap.Error(err))
		return err
	}

	stdin, err := s.cmd.StdinPipe()
	if err != nil {
		logger.Error("Failed to attach to stdin", zap.Error(err))
		return err
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			select {
			case in, ok := <-s.input:
				if !ok {
					cerr := stdin.Close()
					if cerr != nil {
						logger.Error("Close error", zap.Error(cerr))
					}
					return
				}
				_, werr := stdin.Write(in)
				if werr != nil {
					logger.Error("Failed to write to command stdin", zap.Error(werr))
					werr = s.cmd.Process.Kill()
					if werr != nil {
						logger.Error("Close error", zap.Error(werr))
					}
					return
				}
			case <-s.quit:
				return
			case <-time.After(s.timeout):
				s.sendError(s.ctx, -1, errors.New("Command execute timeout"))
				cerr := s.cmd.Process.Kill()
				if cerr != nil {
					logger.Error("Close error", zap.Error(cerr))
				}
				return
			}
		}
	}()

	err = s.cmd.Start()
	if err != nil {
		logger.Error("Failed to start command", zap.Error(err))
	}

	return err
}

type executeStream struct {
	Stdout []byte
	Stderr []byte
}

// Start reading the output from the previously spawned process. Runs until command completes and all output of stdout
// and stderr has been read.
func (s *session) runCommand() {
	logger := adlog.MustFromContext(s.ctx)
	defer s.sendClose()

	done := make(chan struct{})
	defer s.wg.Done()

	buf := make([]byte, 32*1024)

	for {
		n, err := s.stdout.Read(buf)
		if err != nil {
			if err != io.EOF {
				logger.Error("read error", zap.Error(err))
			}
			break
		}
		logger.Info("read output")
		output := &executeStream{}
		output.Stdout = buf[:n]
		select {
		case s.output <- adio.ObjectToJson(s.ctx, output, adio.JsonOptSerializeAllNoIndent):
		case <-s.ctx.Done():
			return
		}
		buf = buf[:]
	}

	for {
		n, err := s.stderr.Read(buf)
		if err != nil {
			if err != io.EOF {
				logger.Error("read error", zap.Error(err))
			}
			break
		}
		output := &executeStream{}
		output.Stderr = buf[:n]
		select {
		case s.output <- adio.ObjectToJson(s.ctx, output, adio.JsonOptSerializeAllNoIndent):
		case <-s.ctx.Done():
			return
		}
		buf = buf[:]
	}

	defer close(done)

	err := s.cmd.Wait()
	if err != nil {
		logger.Info("Command wait error", zap.Error(err))
		if exiterr, ok := err.(*exec.ExitError); ok {
			if status, ok := exiterr.Sys().(syscall.WaitStatus); ok {
				s.sendError(s.ctx, status.ExitStatus(), nil)
				return
			}
		}
		s.sendError(s.ctx, 0, err)
		return

	}
	logger.Info("closed")
	s.sendError(s.ctx, 0, nil)
}

// Open a command session, returns error if command is invalid/not allowed or failure starting the subprocess
func (s *session) openSession(msg *commandMsg) error {
	logger := adlog.MustFromContext(s.ctx)

	cmdStr, errC := shellwords.Parse("bash")
	if errC != nil {
		logger.Error("Command parse failure", zap.Error(errC))
		return errors.New("Command parse failure" + errC.Error())
	}

	logger.Sugar().Infof("Starting command %s", cmdStr)

	// Command has been explicitly enabled by platform developer.
	// NOTE: This does not guarantee that all commands/user input will be validated
	//       because this command starts up a pseudo terminal, user input in future messages
	//       is passed to the subprocess without any validation.
	//			 For example if /bin/bash is enabled this will
	//       launch a bash shell with same privilege as connector process and
	//       any further user input in this session will be passed directly to the bash process.
	s.cmd = exec.Command(cmdStr[0], cmdStr[1:]...) // #nosec

	s.timeout = commandInputDeadline

	var err error
	if msg.Terminal {
		s.pty, err = pty.Start(s.cmd)
		if err != nil {
			logger.Error("Command start failed", zap.Error(err))
			return err
		}
		s.wg.Add(1)
		go s.readPty()

		s.wg.Add(1)
		go s.runTerminal()
	} else {
		err = s.startCommand()
		if err != nil {
			return err
		}
		s.wg.Add(1)
		go s.runCommand()
	}
	return nil
}

type commandMsg struct {
	Terminal bool
	Stream   []byte
}

type terminalmsg struct {
	MsgType string `bson:"MsgType"`

	Stream []byte `bson:"Stream"`
}

// Start a command stream. Returns error on failure to start the command including on invalid input
// or process start failure.
func (p *Command) StreamDelegate(ctx context.Context, inCh, outCh, errCh chan adconnector.Body,
	streamName string, in adconnector.Body) error {
	logger := adlog.MustFromContext(ctx)

	msg := &commandMsg{}
	err := adio.JsonToObjectCtx(ctx, bytes.NewReader(in), msg)
	if err != nil {
		logger.Error("Unable to deserialize command message json", zap.Error(err))
		return err
	}
	logger.Sugar().Infof("New session received: %s", streamName)

	s := session{
		sessionId: streamName,
		ctx:       ctx,
		input:     inCh,
		output:    outCh,
		err:       errCh,
		quit:      make(chan struct{})}
	// Open the command before inserting into the global map
	err = s.openSession(msg)
	if err != nil {
		logger.Error("Failed to open command", zap.Error(err))
	}
	return err
}
