package plugins

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net/http"
	"time"

	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adio"

	"go.uber.org/zap"

	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adconnector"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adlog"
)

type Upgrade struct {
	cancel context.CancelFunc
}

func (u *Upgrade) Init(ctx context.Context) {
}

func (u *Upgrade) Delegate(ctx context.Context, in adconnector.Body) (io.Reader, error) {
	logger := adlog.MustFromContext(ctx)

	if len(in) > (500 << 10) {
		logger.Info("big upgrade message received")
		getDispatcher(ctx).ToggleConnection()
		return nil, nil
	}
	adlog.MustFromContext(ctx).Sugar().Infof("got upgrade request : %s", string(in))
	msg := make(map[string]interface{})
	err := json.Unmarshal(in, &msg)
	if err != nil {
		logger.Error("failed upgrade message unmarshal", zap.Error(err))
		return nil, err
	}

	statusUrl, ok := msg["DownloadStatusLocation"].(string)
	if !ok {
		logger.Error("Failed to get download status location")
		return nil, nil
	}

	go func() {
		logger.Sugar().Infof("sending download status update to : %s", statusUrl)
		_, status, _, err := getDispatcher(ctx).PostRaw(ctx, statusUrl, nil, map[string]interface{}{"DownloadError": nil, "DownloadProgress": 100})
		if err != nil {
			logger.Error("failed to send upgrade status update", zap.Error(err))
		} else if status != http.StatusOK {
			logger.Sugar().Infof("non-ok status returned : %d", status)
		}
	}()

	return nil, nil
}

func (u *Upgrade) Active(ctx context.Context) bool {
	return true
}

func (u *Upgrade) Stop(ctx context.Context) error {
	return nil
}

func (u *Upgrade) doUpgrade(ctx context.Context) {
	logger := adlog.MustFromContext(ctx)

	first := true
	for {
		if !first {
			select {
			case <-time.After(20 * time.Second):
			case <-ctx.Done():
				return
			}
		}
		first = false

		rsp, status, _, err := getDispatcher(ctx).GetRaw("/v1/packagemanagement/ConnectorInstallStatuses", nil)
		if err != nil {
			logger.Error("failed to get upgrade message", zap.Error(err))
			continue
		}

		if status != http.StatusOK {
			logger.Sugar().Infof("non-200 when getting upgrade message : %d", status)
			continue
		}

		var upgradeMessage map[string]interface{}
		err = json.Unmarshal(rsp, &upgradeMessage)
		if err != nil {
			logger.Error("failed to unmarshal upgrade message", zap.Error(err))
			continue
		}

		logger.Sugar().Infof("upgrade message : %+v", upgradeMessage)

		// Report upgrade start
		u.reportUpgradeStatus(ctx, map[string]interface{}{"Status": "InProgress"})

		err = u.downloadImage(ctx, upgradeMessage)
		if err != nil {
			continue
		}
	}
}

var downloadClient *http.Client

func init() {
	tlsConf := &tls.Config{InsecureSkipVerify: true} // #nosec
	transp := &http.Transport{TLSClientConfig: tlsConf}
	downloadClient = &http.Client{Transport: transp}
}

func (u *Upgrade) downloadImage(ctx context.Context, msg map[string]interface{}) error {
	logger := adlog.MustFromContext(ctx)

	// Randomly fail to download
	if rand.Intn(100) > 1 {
		logger.Info("failing upgrade download")
		//u.reportUpgradeStatus(ctx, map[string]interface{}{"DownloadError": map[string]interface{}{"Message": "failed to download!"}})
		u.reportUpgradeStatus(ctx, map[string]interface{}{"DownloadError": "failed to download!"})

		return fmt.Errorf("random fail")
	}

	dloadmsg, ok := msg["DownloadMessage"].(map[string]interface{})
	if !ok {
		logger.Error("failed to get address")
		u.reportUpgradeStatus(ctx, map[string]interface{}{"DownloadError": map[string]interface{}{"Message": "failed to download!"}})
		return fmt.Errorf("no address!")
	}

	url, ok := dloadmsg["RemoteAddress"].(string)
	if !ok {
		logger.Error("failed to get address")
		u.reportUpgradeStatus(ctx, map[string]interface{}{"DownloadError": map[string]interface{}{"Message": "failed to download!"}})
		return fmt.Errorf("no address!")
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		logger.Error("failed to construct request", zap.Error(err))
		u.reportUpgradeStatus(ctx, map[string]interface{}{"DownloadError": map[string]interface{}{"Message": "failed to download!"}})
		return err
	}
	rsp, err := downloadClient.Do(req)
	if err != nil {
		logger.Error("failed to execute download request", zap.Error(err))
		u.reportUpgradeStatus(ctx, map[string]interface{}{"DownloadError": map[string]interface{}{"Message": fmt.Sprintf("failed to download! : %s", err.Error())}})
		return err
	}

	defer func() {
		if cerr := rsp.Body.Close(); cerr != nil {
			logger.Error("failed to close response", zap.Error(cerr))
		}
	}()

	contentLength := rsp.ContentLength

	n, err := io.Copy(ioutil.Discard, rsp.Body)
	if err != nil {
		logger.Error("failed to execute download request", zap.Error(err))
		u.reportUpgradeStatus(ctx, map[string]interface{}{"DownloadError": map[string]interface{}{"Message": fmt.Sprintf("failed to download! : %s", err.Error())}})
		return err
	}

	if n < contentLength {
		err = fmt.Errorf("short read : %d, expected %d", n, contentLength)
		logger.Error("failed to execute download request", zap.Error(err))
		u.reportUpgradeStatus(ctx, map[string]interface{}{"DownloadError": map[string]interface{}{"Message": fmt.Sprintf("failed to download! : %s", err.Error())}})
		return err
	}

	u.reportUpgradeStatus(ctx, map[string]interface{}{"DownloadError": nil, "DownloadProgress": 100})

	logger.Info("image download done")
	return nil
}

func (p *Upgrade) StartOperation(ctx context.Context, in map[string]interface{}) (Operation, error) {
	logger := adlog.MustFromContext(ctx)
	logger.Sugar().Infof("got upgrade operation : %+v", in)
	vers, ok := in["Version"].(string)
	if !ok {
		return nil, fmt.Errorf("no version in request")
	}

	if vers == getDispatcher(ctx).GetVersion() {
		logger.Info("already at version")
		return p, nil
	}

	if p.cancel != nil {
		p.cancel()
	}

	ctx, p.cancel = context.WithCancel(ctx)
	go p.doUpgrade(ctx)

	return p, nil
}

// Send a REST POST to update the devices Intersight install object, reporting is best effort
// if we fail to get a success response from Intersight the error is only logged.
func (p *Upgrade) reportUpgradeStatus(ctx context.Context, update map[string]interface{}) {
	logger := adlog.MustFromContext(ctx)

	// Add device registration to message
	update["DeviceRegistration"] = getDispatcher(ctx).GetIdentity()
	logger.Sugar().Infof("upgrade update : %s", adio.ObjectToJson(ctx, update, nil))
	_, status, _, err := getDispatcher(ctx).PostRaw(ctx, "/v1/packagemanagement/ConnectorInstalls", nil, update)
	if err != nil {
		logger.Error("failed to send upgrade status update", zap.Error(err))
	} else if status != http.StatusOK {
		logger.Sugar().Infof("non-ok status returned : %d", status)
	}
}
