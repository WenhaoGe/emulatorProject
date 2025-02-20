package inventory

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strings"

	"bitbucket-eng-sjc1.cisco.com/an/apollo-test/dcSpewer/config"
	"bitbucket-eng-sjc1.cisco.com/an/apollo-test/dcSpewer/plugins"
	"bitbucket-eng-sjc1.cisco.com/an/apollo-test/dcSpewer/utils"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adlog"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adutil"
	"go.uber.org/zap"
)

// Chassis describes physical chassis object
type Chassis struct {
	Config config.DeviceConfiguration
	Serial string
	plugin *plugins.HttpPlugin
}

// Init Chassis object
func (c *Chassis) Init(config config.DeviceConfiguration) {
	c.Config = config
	if len(c.Config.Serial) == 0 {
		c.Config.Serial = append(c.Config.Serial, "EM"+strings.ToUpper(adutil.NewRandStr(4)))
	}
	c.Serial = c.Config.Serial[0]
}

func (c *Chassis) GetSerial() string {
	return c.Serial
}

func (c *Chassis) GetConfig() config.DeviceConfiguration {
	return c.Config
}

func (c *Chassis) GetMuxPresence(ctx context.Context, plugin *plugins.HttpPlugin, portStr string) ([]byte, error) {
	logger := adlog.MustFromContext(ctx)
	trace := utils.GetTraceFromContext(ctx)
	logger.Sugar().Infof("generating MuxPresence event payload for %s", c.Serial)
	plugin.InventoryDb = c.Config.InventoryDatabase
	chassis, err := plugin.ResolvePath(ctx, fmt.Sprintf("/%s/redfish/v1/Chassis/1/", c.Serial))
	if err != nil {
		logger.Error("redfish response error", zap.Error(err))
		return nil, err
	}
	iom, err := plugin.ResolvePath(ctx,
		fmt.Sprintf("/%s/redfish/v1/Chassis/%s/CMC", c.Serial, strings.ToLower(trace.Node)))
	if err != nil {
		logger.Error("redfish response error", zap.Error(err))
		return nil, err
	}

	plugin.AddItemToIomToChassisMap(iom["SerialNumber"].(string), chassis["SerialNumber"].(string))

	muxPresenceArgs := &MuxPresentPayloadArgs{
		AInMgmtInstance: ArgsBufProperty{Buf: ""},
	}
	muxPresencePayload := EventPayload{
		Type: "REQUEST",
		API:  "SCI_muxPresentCb",
		Args: muxPresenceArgs,
	}

	if strings.ToLower(trace.Node) == "a" {
		muxPresenceArgs.AInModuleNum = 1
	} else {
		muxPresenceArgs.AInModuleNum = 2
	}

	muxPresenceArgs.AInChVendor = ArgsBufProperty{Buf: chassis["Manufacturer"]}
	muxPresenceArgs.AInChSerial = ArgsBufProperty{Buf: chassis["SerialNumber"]}
	muxPresenceArgs.AInChModel = ArgsBufProperty{Buf: chassis["Model"]}
	muxPresenceArgs.AInMuxModel = ArgsBufProperty{Buf: iom["Model"]}
	muxPresenceArgs.AInMuxVendor = ArgsBufProperty{Buf: iom["Manufacturer"]}
	muxPresenceArgs.AInMuxSerial = ArgsBufProperty{Buf: iom["SerialNumber"]}

	fabricPort, err := GeneratePortDetails(portStr, muxPresenceArgs.AInModuleNum, "")
	if err != nil {
		logger.Error("failed generate aInSwitchIntf", zap.Error(err))
	}
	muxPresenceArgs.AInSwitchIntf = []*ArgsIntf{fabricPort}

	iomPort, err := GeneratePortDetails(portStr, muxPresenceArgs.AInModuleNum, iom["Model"].(string))
	if err != nil {
		logger.Error("failed generate aInMuxIntf", zap.Error(err))
	}
	muxPresenceArgs.AInMuxIntf = []*ArgsIntf{iomPort}

	payload, err := json.Marshal(muxPresencePayload)
	logger.Sugar().Infof("MUX Presence PAYLOAD: %s", string(payload))
	if err != nil {
		return nil, err
	}
	return payload, nil
}

// GetNewChassis creates new chassis object
func GetNewChassis(config config.DeviceConfiguration) *Chassis {
	chas := &Chassis{}
	chas.Init(config)
	return chas
}

// GetIOMportNumber uses FI port to generate unique port from IOM side
func GetIOMportNumber(portNum int, iomPortCount int) int {
	// Using Triangle wave to uniquely generate port numbers
	// abs((4*a/p)*abs(((x-p/4)%p)-(p/2))-a)
	// where a is wave amplitutde, and p is period
	a := float64(iomPortCount)
	p := a * 4.0
	intP := iomPortCount * 4
	bigIntP := big.NewInt(int64(intP))
	modArg := big.NewInt(int64((portNum - intP/4)))
	bigIntMod := new(big.Int)
	bigIntMod = bigIntMod.Mod(modArg, bigIntP)
	mod := float64(bigIntMod.Int64())

	port := math.Abs((4.0*a/p)*math.Abs(mod-(p/2.0)) - a)
	if port == 0 {
		return iomPortCount
	}
	return int(port)
}
