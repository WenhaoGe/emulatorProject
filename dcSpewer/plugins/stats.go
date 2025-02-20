package plugins

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"os"
	"time"

	"io"

	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adconnector"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adio"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adlog"
	"go.uber.org/zap"
)

type Stats struct {
	xmlapi *XmlPlugin
}

type statMsg struct {
	StatCollector  string
	TargetResource string
	Fields         []field
}

type field struct {
	FieldName       string
	Transformations []transform
}

type transform struct {
	Translation string
}

var statsEnabled = true

// Mapping between UCS stat class names to their parent class.
// Emulator database file does not contain stat mos, instead
// construct stats from their parent class.
var ucsStatMapping = map[string]string{
	"etherRxStats":           "etherPIo",
	"etherTxStats":           "etherPIo",
	"adaptorVnicStats":       "adaptorHostEthIf",
	"fcStats":                "fcPIo",
	"equipmentPsuStats":      "equipmentPsu",
	"equipmentPsuInputStats": "equipmentPsu",
	//"equipmentRackUnitPsuStats":"equipmentPsu",
	//"equipmentFexPsuInputStats":"equipmentPsu",
	"equipmentFanModuleStats": "equipmentFanModule",
	"equipmentIOCardStats":    "equipmentIOCard",
	"computeMbTempStats":      "computeBoard",
	"computeMbPowerStats":     "computeBoard",
	//"computeRackUnitMbTempStats":"",
}

func init() {
	if os.Getenv("STATS_DISABLE") != "" {
		statsEnabled = false
	}
}

func (p *Stats) Init(ctx context.Context) {
}

func (p *Stats) Delegate(ctx context.Context, in adconnector.Body) (io.Reader, error) {
	return nil, fmt.Errorf("not implemented")
}

func (p *Stats) BatchDelegate(ctx context.Context, in adconnector.Body) ([]map[string]interface{}, error) {
	logger := adlog.MustFromContext(ctx)

	if !statsEnabled {
		return nil, nil
	}

	msg := &statMsg{}
	err := adio.JsonToObjectCtx(ctx, bytes.NewReader(in), msg)
	if err != nil {
		logger.Error("error deserialize", zap.Error(err))
		return nil, err
	}
	logger.Info("collecting stats", zap.String(adlog.LogStimType, msg.StatCollector))

	//logger.Sugar().Infof("got stat collect: %s", msg)

	switch msg.StatCollector {
	case "ConnectorStats":
		return connectorStatCollect(ctx, msg)
	case "XmlStats":
		return p.xmlStatCollect(ctx, msg)
	default:
		// TODO: Support other stats
		return nil, fmt.Errorf("Unknown stat type: %s", msg.StatCollector)
	}
}

// TODO: Expand collection
func connectorStatCollect(ctx context.Context, msg *statMsg) ([]map[string]interface{}, error) {
	out := make(map[string]interface{})

	out["timeStamp"] = time.Now().UTC().UnixNano()
	out["WsTxBytes"] = 50
	out["WsRxBytes"] = 40

	return []map[string]interface{}{out}, nil
}

// TODO: Expand collection
func (p *Stats) xmlStatCollect(ctx context.Context, msg *statMsg) ([]map[string]interface{}, error) {
	var out []map[string]interface{}
	logger := adlog.MustFromContext(ctx)

	//logger.Info("in xml stat collect")
	switch msg.TargetResource {
	/*case "etherTxStats":
		logger.Info("ether tx stats")
		for j := 0; j < 2; j++ {
			for i := 0; i < 100; i++ {
				if rand.Intn(100) > 50 {
					continue
				}
				dn := "sys/switch-"
				if j > 0 {
					dn += "B"
				} else {
					dn += "A"
				}

				dn += "/slot-1/switch-ether/port-" + strconv.Itoa(i) + "/tx-stats"
				o := map[string]interface{}{
					"dn":        dn,
					"bytesTx":   rand.Int() % 100,
					"timeStamp": time.Now().UTC().UnixNano(),
				} // #nosec
				out = append(out, o)
			}
		}

	case "etherRxStats":
		logger.Info("ether rx stats")
		for j := 0; j < 1; j++ {
			for i := 0; i < 100; i++ {
				if rand.Intn(100) > 50 {
					continue
				}
				dn := "sys/switch-"
				if j > 0 {
					dn += "B"
				} else {
					dn += "A"
				}

				dn += "/slot-1/switch-ether/port-" + strconv.Itoa(i) + "/rx-stats"
				o := map[string]interface{}{
					"dn":        dn,
					"bytesTx":   rand.Int() % 100,
					"timeStamp": time.Now().UTC().UnixNano(),
				} // #nosec
				out = append(out, o)
			}
		}
	case "equipmentPsuStats":
		for i := 0; i < 100; i++ {
			if rand.Intn(100) > 50 {
				continue
			}
			dn := "sys/chassis-" + strconv.Itoa(i%15) + "/psu-" + strconv.Itoa(i%4) + "/stats"
			o := map[string]interface{}{
				"dn":             dn,
				"ambientTemp":    rand.Int() % 65,
				"energyConsumed": rand.Int() % 20,
				"timeStamp":      time.Now().UTC().UnixNano(),
			} // #nosec
			out = append(out, o)
		}*/
	case "computeRackUnit":
		fallthrough
	case "computeBlade":
		if p.xmlapi == nil {
			return nil, nil
		}
		mos := p.xmlapi.ResolveClass(ctx, msg.TargetResource)
		for _, v := range mos {
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
				o := make(map[string]interface{})
				for _, field := range msg.Fields {
					for _, trans := range field.Transformations {
						o[trans.Translation] = mo["-"+field.FieldName]
					}
				}

				for k, v := range o {
					if vs, ok := v.(string); ok {
						switch vs {
						case "on":
							o[k] = 1
						case "off":
							o[k] = 0
						}
					}
				}
				o["dn"] = mo["-dn"]
				o["timeStamp"] = time.Now().UTC().UnixNano()
				out = append(out, o)
			}
		}

	default:
		parentclass, ok := ucsStatMapping[msg.TargetResource]
		if !ok {
			logger.Sugar().Infof("unknown stat class : %s", msg.TargetResource)
			return nil, nil
		}
		mos := p.xmlapi.ResolveClass(ctx, parentclass)
		logger.Sugar().Infof("resolved %s / %+v", parentclass, mos)
		for _, v := range mos {
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
				o := make(map[string]interface{})
				for _, field := range msg.Fields {
					for _, trans := range field.Transformations {
						// Assume everything is a number
						o[trans.Translation] = rand.Int() % 65 // #nosec
					}
				}
				o["dn"] = fmt.Sprintf("%s/stats", mo["-dn"])
				o["timeStamp"] = time.Now().UTC().UnixNano()
				out = append(out, o)
			}
		}
	}

	logger.With(zap.String(adlog.LogStimType, msg.TargetResource)).Sugar().Infof("sending back: %+v", out)
	return out, nil
}
