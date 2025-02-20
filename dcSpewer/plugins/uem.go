package plugins

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"bitbucket-eng-sjc1.cisco.com/an/apollo-test/dcSpewer/utils"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adconnector"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adio"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adlog"
	"go.uber.org/zap"
)

type UemPluginRequest struct {
	ClassId    string
	ObjectType string
	EpInfo     UemEpInfo
	MessageId  string
	Payload    []byte
	Parameters string
}

type UemEpInfo struct {
	ClassId              string
	ObjectType           string
	ConnectionStatus     string
	EpType               string
	Ip                   string
	MacAddress           string
	MemberIdentity       string
	Model                string
	Serial               string
	ServerRegistrationId string
	Vendor               string
}

type UemInventoryResponse struct {
	Base_type struct {
		BaseMo struct {
			Id                    string
			ObjectType            string
			ClassId               string
			ObjectSchemaVersion   string
			UpsertTraceId         string
			Rev                   int
			CreateTime            string
			ModTime               string
			Tags                  []interface{}
			Owners                []interface{}
			SharedScope           string
			InheritPropToOwnerMap map[string]interface{}
			AccountMoid           string
			DomainGroupMoid       string
			Ancestors             []interface{}
			Parent                interface{}
			OldMo                 interface{}
			DisplayNames          map[string]interface{}
			VersionContext        interface{}
			DirtyProps            map[string]interface{}
			LifeCycleState        int
			PermissionResources   []interface{}
			DynamicSecureProps    []interface{}
		}
		DeviceMoId string
		Dn         string
		Rn         string
	}
	Gateway         string
	HostName        string
	IpAddress       string
	Ipv4Address     string
	Ipv4Gateway     string
	Ipv4Mask        string
	Ipv6Address     string
	Ipv6Gateway     string
	Ipv6Prefix      int
	MacAddress      string
	Mask            string
	SwitchId        string
	UemConnStatus   string
	VirtualHostName string
}

type UemPlugin struct {
}

func (p *UemPlugin) Init(ctx context.Context) {
}

func randomHex(n int) string {
	bytes := make([]byte, n)
	if _, err := rand.Read(bytes); err != nil {
		return "a148"
	}
	return hex.EncodeToString(bytes)
}

func generateIpv6() string {
	return fmt.Sprintf("%s::%s:%s:%s:%s", randomHex(2), randomHex(2),
		randomHex(2), randomHex(2), randomHex(2))
}

func (p *UemPlugin) Delegate(ctx context.Context, in adconnector.Body) (io.Reader, error) {
	var out io.Reader
	logger := adlog.MustFromContext(ctx)
	logger.Sugar().Infof("uem plugin got: %s", string(in))

	msg := &UemPluginRequest{}
	inReader := bytes.NewReader(in)
	err := adio.JsonToObjectCtx(ctx, inReader, msg)
	if err != nil {
		return nil, err
	}

	switch msg.ObjectType {
	case "connector.UemStartInventory":
		out, err = p.processStartInventory(ctx, msg)
		if err != nil {
			logger.Error("Warn: processStartInventory failed", zap.Error(err))
			return nil, err
		}
	default:
		out = bytes.NewBuffer(in)
	}

	return out, nil
}

func (p *UemPlugin) processStartInventory(ctx context.Context, msg *UemPluginRequest) (io.Reader, error) {
	logger := adlog.MustFromContext(ctx)
	trace := utils.GetTraceFromContext(ctx)
	node := trace.ParentNode

	if node == "" && trace.Node == "" {
		node = "A"
	} else if node == "" {
		node = trace.Node
	}

	resp := UemInventoryResponse{}
	resp.Base_type.BaseMo.ObjectType = "management.Interface"
	resp.Base_type.BaseMo.ClassId = "management.Interface"
	resp.Base_type.DeviceMoId = msg.EpInfo.ServerRegistrationId
	resp.SwitchId = node
	resp.UemConnStatus = "connected"

	epType := msg.EpInfo.EpType
	if epType == "" {
		if strings.Contains(msg.EpInfo.Model, "IOM") {
			epType = "CMC"
		} else if strings.Contains(msg.EpInfo.Model, "MLOM") || (strings.Contains(msg.EpInfo.Model, "VIC")) {
			epType = "VIC"
		} else if strings.Contains(msg.EpInfo.Model, "-M5") {
			epType = "BladeMC"
		}
	}

	switch epType {
	case "VIC":
		resp.Base_type.Dn = fmt.Sprintf("/redfish/v1/Chassis/CMC/%s_%s/mgmt/if",
			msg.EpInfo.Model, msg.EpInfo.Serial)
	case "CMC":
		resp.Base_type.Dn = fmt.Sprintf("/redfish/v1/Chassis/Unknown/NetworkAdapters/%s_%s/mgmt/%s/if",
			msg.EpInfo.Model, msg.EpInfo.Serial, strings.ToUpper(node))
	case "BladeMC":
		resp.Base_type.Dn = fmt.Sprintf("/redfish/v1/Systems/%s/mgmt/if", msg.EpInfo.Serial)
	}

	switch trace.Platform {
	case IMCBladePlatformType, Imcm4PlatformType, Imcm5PlatformType:
		resp.IpAddress = "127.0.0.1"
	default:
		resp.IpAddress = trace.Ip[0]
	}

	respBytes, err := json.Marshal(resp)
	logger.Sugar().Infof("uem plugin repsonding with: %s", string(respBytes))
	return bytes.NewBuffer(respBytes), err
}
