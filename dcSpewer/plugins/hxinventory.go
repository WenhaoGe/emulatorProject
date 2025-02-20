package plugins

import (
	"context"
	"encoding/json"
	"math/rand"
	"strings"

	"bytes"
	"io"

	"bitbucket-eng-sjc1.cisco.com/an/apollo-test/dcSpewer/storage"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adconnector"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adio"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adlog"
	"go.uber.org/zap"
)

type HxInventory struct {
}

func (p *HxInventory) Init(ctx context.Context) {

}

type hxInventoryMessage struct {
	ApiName string
}

func (p *HxInventory) Delegate(ctx context.Context, in adconnector.Body) (io.Reader, error) {
	logger := adlog.MustFromContext(ctx)
	logger.Sugar().Infof("got req: %s", string(in))
	msg := &hxInventoryMessage{}
	err := adio.JsonToObjectCtx(ctx, bytes.NewReader(in), msg)
	if err != nil {
		return nil, err
	}

	inv, err := storage.GetEmulatorStorage(ctx).GetInventory(ctx, msg.ApiName)
	if err != nil {
		logger.Error("failed to get inventory from storage", zap.Error(err))
		return nil, err
	} else {
		logger.Sugar().Debugf("got inventory : %s", inv.String())

		// TODO: Optimize/generalize this and allow clients to customize
		if strings.Contains(msg.ApiName, "alarm") {
			logger.Info("special alarm handling")
			var invJson map[string]interface{}
			dec := json.NewDecoder(inv)
			err = dec.Decode(&invJson)
			if err != nil {
				logger.Error("failed alarm json unmarshal", zap.Error(err))
				return inv, nil
			}
			alarmData, ok := invJson[msg.ApiName].([]interface{})
			if !ok {
				logger.Info("alarm data not found")
			} else {
				// Re-slice the alarms to remove random alarms
				i := 0
				for _, alarm := range alarmData {
					// Randomly drop some
					if rand.Intn(100) > 25 {
						// Randomly update
						if rand.Intn(100) > 30 {
							if alarmJson, ok := alarm.(map[string]interface{}); ok {
								st := rand.Intn(4)
								switch st {
								case 0:
									alarmJson["status"] = "CRITICAL"
								case 1:
									alarmJson["status"] = "INFO"
								case 2:
									alarmJson["status"] = "WARNING"
								case 3:
									alarmJson["status"] = "CLEARED"
								}

							} else {
								logger.Info("alarm json not found")
							}
						}

						alarmData[i] = alarm
						i++

					}

				}
				alarmData = alarmData[:i]
				invJson[msg.ApiName] = alarmData
				inv.Reset()
				enc := json.NewEncoder(inv)
				err = enc.Encode(invJson)
				return inv, err
			}
		}
	}

	return inv, nil
}
