package plugins

import (
	"bytes"
	"context"
	"fmt"
	"net/http"

	"io"

	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adconnector"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adio"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adlog"
)

type sdCardDownloadMessage struct {
	AuthToken string

	DownloadUrl string

	Md5sum string

	PartitionName string

	RemoteFile string

	RemotePassword string

	RemoteShare string

	RemoteUsername string

	RequestType string

	SslCert string

	StatusInterval int

	StatusMoId string
}

type DownloadStatusMessage struct {
	DownloadFilePath   string
	DownloadStage      string
	DownloadFileSize   int
	DownloadError      string
	DownloadPercentage int
}

type SdCardDownload struct {
	sem chan struct{}
}

func (p *SdCardDownload) Init(ctx context.Context) {
	p.sem = make(chan struct{}, 1)

}

func (p *SdCardDownload) Delegate(ctx context.Context, in adconnector.Body) (io.Reader, error) {
	logger := adlog.MustFromContext(ctx)
	logger.Sugar().Infof("sd card download got: %s", string(in))

	msg := &sdCardDownloadMessage{}
	err := adio.JsonToObjectCtx(ctx, bytes.NewReader(in), msg)
	if err != nil {
		return nil, err
	}

	switch msg.RequestType {
	case "TriggerImageDownload":
		select {
		case p.sem <- struct{}{}:
		default:
			return nil, fmt.Errorf("download already in progress")
		}
		// Send back success status
		// TODO: Actually try download
		go func() {
			defer func() {
				<-p.sem
			}()

			// Two status one at 50% next 100%
			status := make(map[string]interface{})
			status["DownloadStage"] = "DOWNLOADING"
			status["DownloadPercentage"] = 50
			err = getDispatcher(ctx).SendRestStim(ctx, status, http.MethodPatch, msg.StatusMoId)
			if err != nil {
				logger.Sugar().Errorf("failed to send download status update: %s", err.Error())
				return
			}

			status["DownloadStage"] = "NONE"
			status["DownloadPercentage"] = 100
			err = getDispatcher(ctx).SendRestStim(ctx, status, http.MethodPatch, msg.StatusMoId)
			if err != nil {
				logger.Sugar().Errorf("failed to send download status update: %s", err.Error())
				return
			}
		}()
	default:
		return nil, fmt.Errorf("unknown request type")
	}
	return nil, nil
}
