package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"bitbucket-eng-sjc1.cisco.com/an/apollo-test/dcSpewer/utils"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adconnector"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adio"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adlog"
	"go.uber.org/zap"
)

const defultRedfishEndpoint = "redfish.default.svc.cluster.local:8000"
const apiResponseLimit = 5 * 1024 * 1024

type HttpPluginRequest struct {
	ClassId          string
	ObjectType       string
	EncryptedAesKey  string
	EncryptionKey    string
	SecureProperties interface{}
	Body             []byte
	DialTimeout      int
	Header           interface{}
	Internal         bool
	Method           string
	Timeout          int
	Url              url.URL
}

type RequestUrl struct {
	ClassId    string
	ObjectType string
	ForceQuery bool
	Fragment   string
	Host       string
	Opaque     string
	Path       string
	RawPath    string
	RawQuery   string
	Scheme     string
}

type HttpPluginResponse struct {
	Body       []byte
	StatusCode int
	Header     []byte
	Status     string
}

type HttpPlugin struct {
	client            *http.Client
	RedfishEndpoint   string
	InventoryDb       string
	ChassisSerials    map[int]string
	IomToChassisMap   map[string]string
	mu                sync.Mutex
	additionalHeaders map[string]string
}

type EpMessage struct {
	ClassId      string
	ObjectType   string
	Address      string
	Id           string
	MessageId    string
	Model        string
	MsgBody      string
	RoutePath    string
	SerialNumber string
	Timeout      int
	Type         string
	Vendor       string
}

type EpProxyHttpResult struct {
	*HttpPluginResponse
	ClassId    string
	ObjectType string
}

type EpMessageBody struct {
	EpMessage
	Messages []EpMessage
}

type EpProxyResult struct {
	EpMessage
	Result EpProxyHttpResult
}

type EpProxyResponse struct {
	EpMessage
	Messages []EpProxyResult
}

func (p *HttpPlugin) Init(ctx context.Context) {
	p.client = &http.Client{}
	p.additionalHeaders = nil
	if endpoint, ok := os.LookupEnv("REDFISH_ENDPOINT"); ok {
		p.RedfishEndpoint = endpoint
	} else {
		p.RedfishEndpoint = defultRedfishEndpoint
	}
}

func (p *HttpPlugin) SetAdditionalHeaders(headers map[string]string) {
	p.additionalHeaders = headers
}

func (p *HttpPlugin) AddItemToChassisSerial(key int, serial string) {
	if p.ChassisSerials == nil {
		p.ChassisSerials = make(map[int]string)
	}
	p.ChassisSerials[key] = serial
}

func (p *HttpPlugin) AddItemToIomToChassisMap(iomSerial string, chassisSerial string) {
	if p.IomToChassisMap == nil {
		p.IomToChassisMap = make(map[string]string)
	}
	p.IomToChassisMap[iomSerial] = chassisSerial
}

func (p *HttpPlugin) MakeRedfishRestAPIcall(ctx context.Context, reqMethod string, reqUrl string, reqBody io.Reader) (*http.Response, error) {
	logger := adlog.MustFromContext(ctx)

	req, err := http.NewRequestWithContext(ctx, reqMethod, reqUrl, reqBody)
	req.Header.Add("PID", p.InventoryDb)

	if p.additionalHeaders != nil {
		for k, v := range p.additionalHeaders {
			req.Header.Add(k, v)
		}
	}

	if err != nil {
		logger.Error("Failed to construct http request", zap.Error(err))
		return nil, err
	}

	logger.Sugar().Infof("Sending request to redfish API: %s method: %s, body: %s", reqUrl, reqMethod, reqBody)
	resp, err := p.client.Do(req)
	return resp, err
}

func decodeRedfishResponse(resp *http.Response) (map[string]interface{}, error) {
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	response := make(map[string]interface{})
	err = json.Unmarshal(body, &response)
	return response, err
}

func (p *HttpPlugin) ResolvePath(ctx context.Context, path string) (map[string]interface{}, error) {
	redfishUrl := url.URL{
		Host:   p.RedfishEndpoint,
		Scheme: "http",
	}

	redfishUrl.Path = path
	resp, err := p.MakeRedfishRestAPIcall(ctx, "GET", redfishUrl.String(), nil)
	if err != nil {
		return nil, err
	}
	return decodeRedfishResponse(resp)
}

func (p *HttpPlugin) getResponse(ctx context.Context, msg *HttpPluginRequest) (*HttpPluginResponse, error) {
	var resp *http.Response
	var err error
	retryCount := 0
	for {
		resp, err = p.MakeRedfishRestAPIcall(ctx, msg.Method, msg.Url.String(), bytes.NewReader(msg.Body))
		if err == nil {
			break
		}

		if err != nil && retryCount == 2 {
			return nil, err
		}
		time.Sleep(1 * time.Second)
		retryCount++
	}

	defer resp.Body.Close()

	response := HttpPluginResponse{
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		Header:     adio.ObjectToJson(ctx, resp.Header, adio.JsonOptSerializeAllNoIndent),
	}

	body, err := ioutil.ReadAll(resp.Body)
	response.Body = body
	if err != nil {
		return nil, err
	}

	return &response, nil
}

func (p *HttpPlugin) Delegate(ctx context.Context, in adconnector.Body) (io.Reader, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var response *HttpPluginResponse

	logger := adlog.MustFromContext(ctx)
	logger.Sugar().Infof("http plugin got: %s", string(in))
	trace := utils.GetTraceFromContext(ctx)

	msg := &HttpPluginRequest{}
	err := adio.JsonToObjectCtx(ctx, bytes.NewReader(in), msg)

	if err != nil {
		return nil, err
	}

	msg.Url.Host = p.RedfishEndpoint
	//If the request is for EP proxy on the FI parse the request and send it directly to redfish emulator
	if msg.Url.Path == "/EndpointProxy" {
		epReq := &HttpPluginRequest{}
		epReq.Url.Host = p.RedfishEndpoint

		response = &HttpPluginResponse{
			StatusCode: 200,
			Status:     "200",
		}

		msgBody := EpMessageBody{}
		json.Unmarshal(msg.Body, &msgBody)

		epResponse := EpProxyResponse{
			EpMessage: EpMessage{
				ClassId:    "connector.EndpointMessageSet",
				ObjectType: "connector.EndpointMessageSet",
			},
			Messages: []EpProxyResult{},
		}

		for _, epReqMsg := range msgBody.Messages {
			if epReqMsg.Id != "" {
				// Request for Chassis
				chassisId, err := strconv.Atoi(epReqMsg.Id)
				if err != nil {
					return nil, err
				}
				chassisSerial, ok := p.ChassisSerials[chassisId]
				if !ok {
					return nil, fmt.Errorf("chassis with id %d doesn't exist in HttpPlugin map", chassisId)
				}

				if epReqMsg.RoutePath == "/redfish/v1/Chassis/CMC" {
					epReqMsg.RoutePath = fmt.Sprintf("%s/%s/CMC",
						"/redfish/v1/Chassis", strings.ToLower(trace.Node))
				}
				epReqMsg.RoutePath = fmt.Sprintf("/%s%s", chassisSerial, epReqMsg.RoutePath)
			} else if epReqMsg.ObjectType == "connector.AdapterEndpointRequest" {
				// Request for a blade server
				// expecting the path to be /redfish/v1/Chassis/EMU8170D6AB/NetworkAdapters/..
				// thus 4th index will be the serial of the server
				pathParts := strings.Split(epReqMsg.RoutePath, "/")
				epReqMsg.RoutePath = fmt.Sprintf("/%s%s",
					pathParts[4],
					epReqMsg.RoutePath)
			}

			epReq.Url.Path = epReqMsg.RoutePath
			epReq.Url.Scheme = "http"
			epReq.Method = epReqMsg.Type
			epReq.Body = []byte(epReqMsg.MsgBody)

			resp, err := p.getResponse(ctx, epReq)
			if err != nil {
				return nil, err
			}

			epResponse.Messages = append(epResponse.Messages, EpProxyResult{
				EpMessage: EpMessage{
					MessageId:  epReqMsg.MessageId,
					ClassId:    "connector.EndpointResult",
					ObjectType: "connector.EndpointResult",
				},
				Result: EpProxyHttpResult{
					HttpPluginResponse: resp,
					ClassId:            "connector.HttpResult",
					ObjectType:         "connector.HttpResult"},
			})
		}

		response.Body, err = json.Marshal(epResponse)
		if err != nil {
			return nil, err
		}

	} else {
		msg.Url.Path = fmt.Sprintf("/%s%s", trace.Serial[0], msg.Url.Path)
		response, err = p.getResponse(ctx, msg)
		if err != nil {
			return nil, err
		}
	}

	jsonResponse, err := json.Marshal(response)
	if err != nil {
		return nil, err
	}

	logger.Sugar().Infof("http plugin resp: %s", string(jsonResponse))
	return strings.NewReader(string(jsonResponse)), nil
}
