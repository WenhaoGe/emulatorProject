package plugins

import (
	"context"
	"io"
	"strings"

	"bitbucket-eng-sjc1.cisco.com/an/apollo-test/dcSpewer/utils"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adconnector"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adlog"
)

const UcsfiPlatformType = "UCSFI"
const UcsfiismPlatformType = "UCSFIISM"
const Imcm5PlatformType = "IMCM5"
const Imcm4PlatformType = "IMCM4"
const HxPlatformType = "HX"
const ChassisType = "CHASSIS"
const IMCBladePlatformType = "IMCBlade"

type PluginManager struct {
	Plugins map[string]Plugin
	nodeid  string
}

type Plugin interface {
	Init(ctx context.Context)

	Delegate(ctx context.Context, in adconnector.Body) (io.Reader, error)
}

type OperationsPlugin interface {
	// Start an operation defined in the input message. Input message is a concrete complex type
	// that is understood by the OperationPlugin.
	StartOperation(ctx context.Context, in map[string]interface{}) (Operation, error)
}

type Operation interface {
	// Returns whether the operation is still active/running. Used by framework to monitor the state
	// of operations.
	//
	// If an operation has failed/stopped and Active returns false the framework will attempt to restart
	// it on the next operations poll.
	Active(ctx context.Context) bool

	// Called by framework to stop the operation. Implementations should stop any routines started for
	// the operation and clean up any resources related to it.
	Stop(ctx context.Context) error
}

func getDispatcher(ctx context.Context) utils.Dispatcher {
	return ctx.Value(dispKey).(utils.Dispatcher)
}

type dispKeyType int

const dispKey dispKeyType = 1

func CreateDispatcherContext(ctx context.Context, dispatcher utils.Dispatcher) context.Context {
	return context.WithValue(ctx, dispKey, dispatcher)
}

func GetPluginManager(ctx context.Context, platform string, inventoryDb string, nodeid string) *PluginManager {
	p := &PluginManager{nodeid: nodeid}

	p.Plugins = make(map[string]Plugin)
	parentNode := strings.ToLower(utils.GetTraceFromContext(ctx).ParentNode)

	// Add default plugins
	jobPlug := &JobPlugin{Platform: p, Nodeid: nodeid}
	p.Plugins["TypeJobStim"] = jobPlug
	p.Plugins["TypeStreamer"] = &StreamPlugin{Platform: p}
	p.Plugins["TypeStats"] = &Stats{}
	p.Plugins["TypeCommand"] = &Command{}
	p.Plugins["TypeCommonInventory"] = &CommonInventory{}
	p.Plugins["TypeUpgrade"] = &Upgrade{}
	p.Plugins["AsyncWorkPlugin"] = &AsyncWork{Platform: p}

	switch platform {
	case Imcm5PlatformType, Imcm4PlatformType:
		if parentNode != "a" && parentNode != "b" {
			p.Plugins["TypeXmlApiNoAuth"] = &XmlPlugin{jobPlugin: jobPlug}
			p.Plugins["TypeEvtChannelCtrlMsg"] = &EventChannel{xmlapi: p.Plugins["TypeXmlApiNoAuth"].(*XmlPlugin)}
			p.Plugins["TypeStats"].(*Stats).xmlapi = p.Plugins["TypeXmlApiNoAuth"].(*XmlPlugin)
			p.Plugins["TypeEndPointProxy"] = &XmlPlugin{jobPlugin: jobPlug}
		}
		p.Plugins["TypeSdCardImageDownload"] = &SdCardDownload{}
		p.Plugins["TypeHttp"] = &HttpPlugin{InventoryDb: inventoryDb}
		p.Plugins["TypeUem"] = &UemPlugin{}
	case UcsfiPlatformType:
		p.Plugins["TypeXmlApiNoAuth"] = &XmlPlugin{}
		p.Plugins["TypeEvtChannelCtrlMsg"] = &EventChannel{xmlapi: p.Plugins["TypeXmlApiNoAuth"].(*XmlPlugin)}
		p.Plugins["TypeStats"].(*Stats).xmlapi = p.Plugins["TypeXmlApiNoAuth"].(*XmlPlugin)
	case HxPlatformType:
		p.Plugins["TypeHxApi"] = &HxInventory{}
	case UcsfiismPlatformType:
		p.Plugins["TypeHttp"] = &HttpPlugin{InventoryDb: inventoryDb}
		p.Plugins["TypeNetworkAgent"] = &NetworkAgent{httpPlugin: p.Plugins["TypeHttp"].(*HttpPlugin)}
		p.Plugins["TypeUem"] = &UemPlugin{}
	case IMCBladePlatformType:
		p.Plugins["TypeHttp"] = &HttpPlugin{InventoryDb: inventoryDb}
		p.Plugins["TypeUem"] = &UemPlugin{}
	}

	for _, plug := range p.Plugins {
		plug.Init(ctx)
	}
	return p
}

func (p *PluginManager) Delegate(ctx context.Context, messageType string, in adconnector.Body) (io.Reader, error) {
	logger := adlog.MustFromContext(ctx)

	//logger.Sugar().Infof("got plugin: %s", messageType)
	//logger.Sugar().Infof("message: %s", string(in))

	// First check if message is defined in debug trace.
	resp := utils.GetTraceResponse(ctx, messageType, in)
	if resp != nil {
		logger.Sugar().Infof("responding from trace map")
		return resp, nil
	}

	if plug, ok := p.Plugins[messageType]; ok {
		return plug.Delegate(ctx, in)
	}
	logger.Sugar().Infof("got unknown plugin: %s", messageType)
	return nil, nil
}
