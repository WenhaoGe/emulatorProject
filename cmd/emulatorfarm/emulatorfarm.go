package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adio"
	v1 "k8s.io/api/apps/v1"

	"flag"

	"bitbucket-eng-sjc1.cisco.com/an/apollo-test/dcSpewer/config"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adlog"
	"github.com/gorilla/mux"
	"go.uber.org/zap"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Copied from apollo/api
// Object returned by the /Systems API exposed by the device connector rest interface.
// Describes the current state and configuration of the devices connection to Intersight
type SystemMo struct {
	// Describes whether the devices connection to Intersight has been enabled by the user
	// Mapped to base.persistantConfig.CloudEnabled
	AdminState bool
	// Describes whether the device is operating in read only mode
	// Mapped to base.persistantConfig.ReadOnlyMode
	ReadOnlyMode bool

	// The state of the devices connection to Intersight
	ConnectionState string
	// Provides additional information on any errors in establishing the connection to Intersight
	ConnectionStateQualifier string

	// The current claim state of this device
	AccountOwnershipState string
	// The Intersight user that claimed the device to their account. Empty for unclaimed devices
	AccountOwnershipUser string
	// The time at which the user performed the claim operation. Time is in UTC and will golang time.Zero for unclaimed devices
	AccountOwnershipTime time.Time
}

type DeviceIdentifierMo struct {
	Id string
}

type VersionMo struct {
	Version string
}

type SecurityTokenMo struct {
	Token    string
	Duration int // seconds
}

type emulatorStatus struct {
	Status        string `json:",omitempty"`
	ClaimedStatus string `json:",omitempty"`
	Identity      string `json:",omitempty"`
	Cloud         string `json:",omitempty"`
	Platform      string `json:",omitempty"`
	Version       string `json:",omitempty"`
	Config        string `json:",omitempty"`

	SecurityToken    string `json:",omitempty"`
	DeviceIdentifier string `json:",omitempty"`

	podIp string

	EmulatedConnections []emuConnections `json:",omitempty"`
}

type connectionState struct {
	NodeId           string
	ConnectionStatus string
}

type emuConnections struct {
	ConnectionStatuses []connectionState
	Id                 string

	Account          string `json:",omitempty"`
	Token            string `json:",omitempty"`
	DeviceIdentifier string `json:",omitempty"`

	ErrorState string `json:",omitempty"`
}

type emulatorConfig struct {
	Name       string
	Platform   string
	Version    string
	Cloud      string
	Status     emulatorStatus
	OnPremIp   string
	OnPremHost bool
	Count      int

	Options map[string]string

	DeviceConfig *config.DeviceConfiguration

	Memory string
	CPU    string
}

type emulatorIdentity struct {
	Identity string
	Platform string
}

var platormRepos map[string]string

var certificates map[string]string

var certLock sync.RWMutex

const internalError = "Internal error"

const apolloTestRepo = "apollo-test"

func init() {
	platormRepos = map[string]string{
		"UCSFI":      "colossus",
		"IMCM5":      "jaguar",
		"HX":         "diesel",
		"SCALE_CONN": apolloTestRepo,
		"Spewer":     apolloTestRepo,
	}

	certificates = make(map[string]string)

}

// Global logger
var logger *zap.Logger

func main() {

	// Log to stdout
	err := flag.Set("log_path", "")
	if err != nil {
		panic(err)
	}

	logger = adlog.InitSimpleLogger(10 << 10 << 10)

	m := mux.NewRouter()
	m.NewRoute().PathPrefix("/Emulators/{id}/{dcapi:.*}").HandlerFunc(executeDcApi)
	//m.HandleFunc("/Emulators/{id}/{dcapi:.*}", executeDcApi)
	m.HandleFunc("/Emulators/{id}", getDeviceStatus).Methods(http.MethodGet)
	m.HandleFunc("/Emulators/{id}", updateEmulator).Methods(http.MethodPost)
	m.HandleFunc("/Emulators", getEmulators).Methods(http.MethodGet)

	m.HandleFunc("/Emulators", createEmulator).Methods(http.MethodPost)

	m.HandleFunc("/Emulators/{id}", deleteEmulator).Methods(http.MethodDelete)
	m.HandleFunc("/Emulators", deleteEmulators).Methods(http.MethodDelete)

	m.HandleFunc("/CreateEmulators", createEmulator)
	m.HandleFunc("/DeleteEmulators/{id}", deleteEmulator)
	m.HandleFunc("/DeleteEmulators", deleteEmulators)
	m.HandleFunc("/Certificates/{id}", getCertificates)
	m.HandleFunc("/Certificates", addCertificate)

	m.HandleFunc("/help", func(w http.ResponseWriter,
		r *http.Request) {
		http.ServeFile(w, r, "/index.html")
	})

	m.HandleFunc("/", func(w http.ResponseWriter,
		r *http.Request) {
		http.ServeFile(w, r, "/index.html")
	})

	http.Handle("/", m)

	log.Fatal(http.ListenAndServe("0.0.0.0:8888", nil))
}

func getKubeClient() *kubernetes.Clientset {
	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err)
	}

	// creates the clientset
	config.Insecure = true
	config.CAFile = ""

	// NewForConfig creates a new Clientset for the given config
	cs, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err)
	}
	return cs
}

func executeDcApi(w http.ResponseWriter, r *http.Request) {
	ns := getNamespace(r)
	if ns == "" {
		logger.Sugar().Infof("no namespace")
		http.Error(w, "No namespace in request, please set in query or header", http.StatusBadRequest)
		return
	}

	v := mux.Vars(r)

	id, ok := v["id"]
	if !ok {
		logger.Info("No id in request")
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	dcapi, ok := v["dcapi"]
	if !ok {
		logger.Info("No api in request")
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	cs := getKubeClient()

	logger.Sugar().Infof("executing dc api: %s", dcapi)

	pod, err := cs.CoreV1().Pods(ns).Get(id, metav1.GetOptions{})
	if err != nil {
		// Retry for the sts pod
		pod, err = cs.CoreV1().Pods(ns).Get(id+"-0", metav1.GetOptions{})
		if err != nil {
			logger.Sugar().Infof("Error getting pods: %s\n", err.Error())
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	if pod.Status.Phase != apiv1.PodRunning {
		logger.Info("Emulator is not running")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	dcapireq, err := http.NewRequest(r.Method, "http://"+pod.Status.PodIP+"/"+dcapi, r.Body)
	if err != nil {
		logger.Sugar().Infof("request create error: %s", err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	dcresp, err := http.DefaultClient.Do(dcapireq)
	if err != nil {
		logger.Sugar().Infof("request do error: %s", err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer func() {
		cerr := dcresp.Body.Close()
		if cerr != nil {
			logger.Error("failed to close body", zap.Error(err))
		}
	}()

	logger.Sugar().Infof("emulator response status : %d", dcresp.StatusCode)
	w.WriteHeader(dcresp.StatusCode)

	_, err = io.Copy(w, dcresp.Body)
	if err != nil {
		logger.Error("failed to copy response", zap.Error(err))
	}

}
func getDeviceStatus(w http.ResponseWriter, r *http.Request) {
	ns := getNamespace(r)
	if ns == "" {
		logger.Sugar().Infof("no namespace")
		http.Error(w, "No namespace in request, please set in query or header", http.StatusBadRequest)
		return
	}

	v := mux.Vars(r)

	id, ok := v["id"]
	if !ok {
		logger.Info("No id in request")
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	getClaim := r.URL.Query().Get("claim")
	cs := getKubeClient()

	pod, err := cs.CoreV1().Pods(ns).Get(id, metav1.GetOptions{})
	if err != nil {
		var err2 error
		pod, err2 = cs.CoreV1().Pods(ns).Get(id+"-0", metav1.GetOptions{})
		if err2 != nil {
			logger.Sugar().Infof("Error getting pods: %s\n", err.Error())
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	if pod.Status.Phase != apiv1.PodRunning {
		logger.Info("Emulator is not running")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	status := &emulatorStatus{
		podIp: pod.Status.PodIP,
	}

	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == "EMU_CLOUD" {
			status.Cloud = e.Value
		} else if e.Name == "DEVICE_CONFIG" {
			status.Config = e.Value
		}
	}

	getEmulatorStatus(status, getClaim != "")

	enc := json.NewEncoder(w)
	enc.SetIndent("", "    ")
	enc.SetEscapeHTML(false)

	err = enc.Encode(status)
	if err != nil {
		logger.Sugar().Infof("Error encoding pod: %s\n", err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

}

const getLimit = 50

func getEmulatorStatus(status *emulatorStatus, getClaim bool) {
	logger.Sugar().Infof("pod ip: %s", status.podIp)
	if status.podIp == "" {
		status.Status = "Emulator initializing"
		return
	}

	versQuery, err := http.NewRequest(http.MethodGet, "http://"+status.podIp+"/Versions", nil)
	if err != nil {
		logger.Sugar().Infof("request create error: %s", err.Error())
		status.Status = internalError
		return
	}

	versQueryResp, err := http.DefaultClient.Do(versQuery)
	if err != nil {
		logger.Sugar().Infof("request do error: %s", err.Error())
		status.Status = internalError
		return
	}
	defer func() {
		cerr := versQueryResp.Body.Close()
		if cerr != nil {
			logger.Error("failed to close body", zap.Error(cerr))
		}
	}()

	vers, err := ioutil.ReadAll(versQueryResp.Body)
	if err != nil {
		logger.Sugar().Infof("vers query error: %s\n", err.Error())
		status.Status = internalError
		return
	}

	if string(vers) == "notreal" {
		// Emulated connection do something different
		var out []emuConnections

		var url string
		if getClaim {
			url = "http://" + status.podIp + "/SecurityTokens"
		} else {
			url = "http://" + status.podIp + "/Systems"
		}
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			logger.Sugar().Infof("request create error: %s", err.Error())
			status.Status = internalError
			return
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			logger.Sugar().Infof("request do error: %s", err.Error())
			status.Status = internalError
			return
		}
		defer func() {
			cerr := resp.Body.Close()
			if cerr != nil {
				logger.Error("failed to close body", zap.Error(err))
			}
		}()
		emuDec := json.NewDecoder(resp.Body)
		err = emuDec.Decode(&out)
		if err != nil {
			logger.Sugar().Infof("Decode error: %s", err.Error())
			status.Status = internalError
			return
		}

		status.EmulatedConnections = out

		return
	} else {

		req, err := http.NewRequest(http.MethodGet, "http://"+status.podIp+"/Systems", nil)
		if err != nil {
			logger.Sugar().Infof("request create error: %s", err.Error())
			status.Status = internalError
			return
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			logger.Sugar().Infof("request do error: %s", err.Error())
			status.Status = internalError
			return
		}
		defer func() {
			cerr := resp.Body.Close()
			if cerr != nil {
				logger.Error("failed to close body", zap.Error(err))
			}
		}()

		var systems []SystemMo
		dec := json.NewDecoder(resp.Body)
		err = dec.Decode(&systems)
		if err != nil {
			logger.Sugar().Infof("Decode error: %s", err.Error())
			status.Status = internalError
			return
		}
		if len(systems) != 1 {
			logger.Sugar().Infof("Invalid response from systems api")
			status.Status = internalError
			return
		}
		status.Status = systems[0].ConnectionState
		status.ClaimedStatus = systems[0].AccountOwnershipState

		resp2, err := http.Get("http://" + status.podIp + "/GetIdentity")
		if err != nil {
			logger.Sugar().Infof("request do error: %s", err.Error())
			status.Status = internalError
			return
		}

		defer func() {
			cerr := resp2.Body.Close()
			if cerr != nil {
				logger.Error("failed to close body", zap.Error(err))
			}
		}()

		id := &emulatorIdentity{}

		dec = json.NewDecoder(resp2.Body)
		err = dec.Decode(id)
		if err != nil {
			logger.Sugar().Infof("Decode error: %s", err.Error())
			status.Status = internalError
			return
		}

		status.Identity = id.Identity
		status.Platform = id.Platform

		var versions []VersionMo
		dec = json.NewDecoder(bytes.NewReader(vers))
		err = dec.Decode(&versions)
		if err != nil {
			logger.Sugar().Infof("Decode error: %s", err.Error())
			status.Status = internalError
			return
		}
		if len(versions) != 1 {
			logger.Sugar().Infof("Invalid response from versions api")
			status.Status = internalError
			return
		}
		status.Version = versions[0].Version
	}

	if getClaim && status.Status == "Connected" {
		diresp, err := http.Get("http://" + status.podIp + "/DeviceIdentifiers")
		if err != nil {
			logger.Sugar().Infof("request read error: %s", err.Error())
			status.Status = internalError
			return
		}

		defer func() {
			cerr := diresp.Body.Close()
			if cerr != nil {
				logger.Error("failed to close body", zap.Error(err))
			}
		}()

		b, err := ioutil.ReadAll(diresp.Body)
		if err != nil {
			logger.Sugar().Infof("request read error: %s", err.Error())
			return
		}
		var di []DeviceIdentifierMo
		err = json.Unmarshal(b, &di)
		if err != nil {
			logger.Sugar().Infof("Decode error: %s", err.Error())
			status.Status = internalError
			return
		}
		if len(di) != 1 {
			logger.Sugar().Infof("Invalid response from device identifiers api")
			return
		}
		status.DeviceIdentifier = di[0].Id

		stresp, err := http.Get("http://" + status.podIp + "/SecurityTokens")
		if err != nil {
			logger.Sugar().Infof("request read error: %s", err.Error())
			return
		}
		defer func() {
			cerr := stresp.Body.Close()
			if cerr != nil {
				logger.Error("failed to close body", zap.Error(err))
			}
		}()
		b, err = ioutil.ReadAll(stresp.Body)
		if err != nil {
			logger.Sugar().Infof("request read error: %s", err.Error())
			return
		}

		var st []SecurityTokenMo
		err = json.Unmarshal(b, &st)
		if err != nil {
			logger.Sugar().Infof("Decode error: %s", err.Error())
			status.Status = internalError
			return
		}
		if len(st) != 1 {
			logger.Sugar().Infof("Invalid response from device identifiers api")
			return
		}
		status.SecurityToken = st[0].Token
	}
}

func getNamespace(r *http.Request) string {
	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		// If not in query look in header
		ns = r.Header.Get("x-namespace")
	}

	return ns
}

func getEmulators(w http.ResponseWriter, r *http.Request) {
	ns := getNamespace(r)
	if ns == "" {
		logger.Sugar().Infof("no namespace")
		http.Error(w, "No namespace in request, please set in query or header", http.StatusBadRequest)
		return
	}

	getClaim := r.URL.Query().Get("claim")

	var skip int
	var err error
	skipRaw := r.URL.Query().Get("skip")
	if skipRaw != "" {
		skip, err = strconv.Atoi(skipRaw)
		if err != nil {
			logger.Sugar().Infof("skip invalid: %s\n", skipRaw)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	}

	cs := getKubeClient()

	pods, err := cs.CoreV1().Pods(ns).List(metav1.ListOptions{})
	if err != nil {
		logger.Sugar().Infof("Error getting pods: %s\n", err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if skip >= len(pods.Items) {
		pods.Items = pods.Items[:0]
	} else {
		pods.Items = pods.Items[skip:]
	}
	if len(pods.Items) > getLimit {
		pods.Items = pods.Items[:getLimit]
	}

	// Statuses map with emulator name as key
	deviceStatuses := make(map[string]*emulatorStatus)

	var wg sync.WaitGroup

	for _, pod := range pods.Items {
		if !strings.HasPrefix(pod.Name, "emulator") {
			continue
		}
		if pod.Status.Phase != apiv1.PodRunning {
			continue
		}
		stsName := strings.TrimRight(pod.Name, "-0")
		deviceStatuses[stsName] = &emulatorStatus{podIp: pod.Status.PodIP}
		for _, e := range pod.Spec.Containers[0].Env {
			if e.Name == "EMU_CLOUD" {
				deviceStatuses[stsName].Cloud = e.Value
			} else if e.Name == "DEVICE_CONFIG" {
				deviceStatuses[stsName].Config = e.Value
			}
		}

		wg.Add(1)
		go func(status *emulatorStatus) {
			defer wg.Done()
			getEmulatorStatus(status, getClaim != "")
		}(deviceStatuses[stsName])
	}

	logger.Info("Wait for queries to complete")
	wg.Wait()
	logger.Info("all queries done")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "    ")
	enc.SetEscapeHTML(false)

	err = enc.Encode(deviceStatuses)
	if err != nil {
		logger.Sugar().Infof("Error encoding pods: %s\n", err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func createEmulator(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	ns := getNamespace(r)
	if ns == "" {
		logger.Sugar().Infof("no namespace")
		http.Error(w, "No namespace in request, please set in query or header", http.StatusBadRequest)
		return
	}

	cs := getKubeClient()

	ens, err := cs.CoreV1().Namespaces().Get(ns, metav1.GetOptions{})
	if err != nil || ens == nil {
		_, err = cs.CoreV1().Namespaces().Create(&apiv1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})
		if err != nil {
			logger.Sugar().Infof("Error creating namespace: %s\n", err.Error())
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	} else {
		logger.Info("namespace exists")
	}

	eConfig := &emulatorConfig{}
	dec := json.NewDecoder(r.Body)
	err = dec.Decode(eConfig)
	if err != nil {
		logger.Sugar().Infof("Json decode error: %s\n", err.Error())
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	repo, ok := platormRepos[eConfig.Platform]
	if !ok {
		logger.Sugar().Infof("Unknown platform\n")
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if repo == apolloTestRepo {
		if eConfig.Count > 1 {
			http.Error(w, "spewers must be spawned one at a time", http.StatusBadRequest)
			return
		}
		eConfig.Count = 1
	}

	envs := []apiv1.EnvVar{
		{
			Name:  "EMU_CLOUD",
			Value: eConfig.Cloud,
		},
		{
			Name: "KUBE_POD_NAME",
			ValueFrom: &apiv1.EnvVarSource{
				FieldRef: &apiv1.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "metadata.name",
				},
			},
		},
	}

	if eConfig.DeviceConfig != nil {
		if repo != "apollo-test" {
			http.Error(w, "DeviceConfig only supported for dc spewer", http.StatusBadRequest)
			return
		}
		b, jerr := json.Marshal(eConfig.DeviceConfig)
		if jerr != nil {
			http.Error(w, "failed to marshal device config : "+jerr.Error(), http.StatusInternalServerError)
			return
		}
		envs = append(envs, apiv1.EnvVar{
			Name:  "DEVICE_CONFIG",
			Value: string(b),
		})
	}

	for k, v := range eConfig.Options {
		envs = append(envs, apiv1.EnvVar{
			Name:  k,
			Value: v,
		})
	}

	if eConfig.Count == 0 {
		eConfig.Count = 1
	}

	var names []string
	inputName := r.URL.Query().Get("podName")
	if inputName != "" {
		names = []string{inputName}
	} else {
		for i := 0; i < eConfig.Count; i++ {
			names = append(names, "emulator-"+eConfig.Name+"-"+newRandStr(8))
		}
	}

	var wg sync.WaitGroup
	for _, name := range names {
		wg.Add(1)
		go func(hostname string) {
			defer wg.Done()
			envs = append(envs, apiv1.EnvVar{
				Name:  "ENV_HOSTNAME",
				Value: hostname,
			})

			serv := &apiv1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name: hostname,
				},
				Spec: apiv1.ServiceSpec{
					Ports: []apiv1.ServicePort{
						{
							Port: 80,
						},
					},
				},
			}

			_, err = cs.CoreV1().Services(ns).Create(serv)
			if err != nil {
				logger.Error("failed to create service", zap.Error(err))
				http.Error(w,
					fmt.Sprintf("failed to create service : %s", err),
					http.StatusInternalServerError)
				return
			}
			sts := &v1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      hostname,
					Namespace: ns,
				},
				Spec: v1.StatefulSetSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": hostname},
					},
					Template: apiv1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Name:      hostname,
							Namespace: ns,
							Labels:    map[string]string{"app": hostname},
						},
						Spec: apiv1.PodSpec{
							NodeSelector: map[string]string{
								"emuNode": "worker",
							},
							Containers: []apiv1.Container{
								{
									ImagePullPolicy: apiv1.PullAlways,
									Name:            "emulator",
									Image:           "dockerhub.cisco.com/cspg-docker/andromeda/" + repo + ":" + eConfig.Version,
									Ports: []apiv1.ContainerPort{
										{
											Name:          "http",
											Protocol:      apiv1.ProtocolTCP,
											ContainerPort: 80,
										},
									},
									Env: envs,
								},
							},
						},
					},
					VolumeClaimTemplates: nil,
					ServiceName:          hostname,
				},
			}

			sts.Kind = "StatefulSet"
			sts.APIVersion = "apps/v1"

			if eConfig.Memory != "" && eConfig.CPU != "" {
				sts.Spec.Template.Spec.Containers[0].Resources = apiv1.ResourceRequirements{
					Limits: apiv1.ResourceList{
						"cpu":    resource.MustParse(eConfig.CPU),
						"memory": resource.MustParse(eConfig.Memory),
					},
					Requests: apiv1.ResourceList{
						"cpu":    resource.MustParse("50m"),
						"memory": resource.MustParse("100Mi"),
					},
				}
			}

			if eConfig.OnPremHost {
				sts.Spec.Template.Spec.HostAliases = []apiv1.HostAlias{
					{
						IP:        eConfig.OnPremIp,
						Hostnames: []string{eConfig.Cloud},
					},
				}
			}

			logger.Sugar().Infof("creating sts : \n%s", adio.ObjectToJson(context.Background(), sts, nil))
			res, poderr := cs.AppsV1().StatefulSets(ns).Create(sts)
			if poderr != nil {
				logger.Error("failed to create sts", zap.Error(poderr))
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			logger.Sugar().Infof("Create pod sts: %v\n", res)
		}(name)
	}

	wg.Wait()

	enc := json.NewEncoder(w)
	err = enc.Encode(&names)
	if err != nil {
		logger.Error("failed to encode response", zap.Error(err))
	}

}

func deleteEmulators(w http.ResponseWriter, r *http.Request) {
	ns := getNamespace(r)
	if ns == "" {
		logger.Sugar().Infof("no namespace")
		http.Error(w, "No namespace in request, please set in query or header", http.StatusBadRequest)
		return
	}

	cs := getKubeClient()

	err := cs.CoreV1().Namespaces().Delete(ns, &metav1.DeleteOptions{GracePeriodSeconds: new(int64)})
	if err != nil {
		logger.Sugar().Infof("Error deleting namespace: %s\n", err.Error())
		http.Error(w, fmt.Sprintf("Error deleting namespace: %s\n", err.Error()), http.StatusInternalServerError)
	}

	/*
		err := cs.AppsV1().StatefulSets(ns).DeleteCollection(&metav1.DeleteOptions{GracePeriodSeconds: new(int64)}, metav1.ListOptions{})
		if err != nil {
			logger.Sugar().Infof("Delete all emulators error: %s\n", err.Error())
			w.WriteHeader(http.StatusInternalServerError)
		}


		err = cs.CoreV1().Services(ns).DeleteCollection(&metav1.DeleteOptions{GracePeriodSeconds: new(int64)}, metav1.ListOptions{})
		if err != nil {
			logger.Sugar().Infof("Delete all services error: %s\n", err.Error())
			w.WriteHeader(http.StatusInternalServerError)
		}

		err = cs.CoreV1().Pods(ns).DeleteCollection(&metav1.DeleteOptions{GracePeriodSeconds: new(int64)}, metav1.ListOptions{})
		if err != nil {
			logger.Sugar().Infof("Delete all emulators error: %s\n", err.Error())
			w.WriteHeader(http.StatusInternalServerError)
		}
	*/
}

func deleteEmulator(w http.ResponseWriter, r *http.Request) {
	ns := getNamespace(r)
	if ns == "" {
		logger.Sugar().Infof("no namespace")
		http.Error(w, "No namespace in request, please set in query or header", http.StatusBadRequest)
		return
	}

	v := mux.Vars(r)

	id, ok := v["id"]
	if !ok {
		logger.Info("No id in request")
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	id = strings.TrimRight(id, "-0")

	cs := getKubeClient()

	bounce := r.URL.Query().Get("bounce")
	if bounce != "" {
		err := cs.CoreV1().Pods(ns).Delete(id+"-0", &metav1.DeleteOptions{GracePeriodSeconds: new(int64)})
		if err != nil {
			logger.Sugar().Infof("Error deleting pod: %s\n", err.Error())
			http.Error(w, fmt.Sprintf("Error deleting pod: %s\n", err.Error()), http.StatusInternalServerError)
			return
		}
		return
	}

	logger.Sugar().Infof("deleting pod: %s\n", id)

	err := cs.AppsV1().StatefulSets(ns).Delete(id, &metav1.DeleteOptions{GracePeriodSeconds: new(int64)})
	if err != nil {
		logger.Sugar().Infof("Error deleting sts: %s\n", err.Error())
		// If error deleting sts, delete pod if it exists.
		err = cs.CoreV1().Pods(ns).Delete(id, &metav1.DeleteOptions{GracePeriodSeconds: new(int64)})
		if err != nil {
			logger.Sugar().Infof("Error deleting pod: %s\n", err.Error())
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	err = cs.CoreV1().Services(ns).Delete(id, &metav1.DeleteOptions{GracePeriodSeconds: new(int64)})
	if err != nil {
		logger.Sugar().Infof("Error deleting service: %s\n", err.Error())
		http.Error(w, fmt.Sprintf("Error deleting service: %s\n", err.Error()), http.StatusInternalServerError)

	}
}

func updateEmulator(w http.ResponseWriter, r *http.Request) {
	ns := getNamespace(r)
	if ns == "" {
		logger.Sugar().Infof("no namespace")
		http.Error(w, "No namespace in request, please set in query or header", http.StatusBadRequest)
		return
	}

	v := mux.Vars(r)

	id, ok := v["id"]
	if !ok {
		logger.Info("No id in request")
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	logger.Sugar().Infof("updating sts: %s\n", id)
	cs := getKubeClient()

	sts, err := cs.AppsV1().StatefulSets(ns).Get(id, metav1.GetOptions{})
	if err != nil {
		logger.Sugar().Infof("Error getting sts: %s\n", err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	eConfig := &emulatorConfig{}
	dec := json.NewDecoder(r.Body)
	err = dec.Decode(eConfig)
	if err != nil {
		logger.Sugar().Infof("Json decode error: %s\n", err.Error())
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// TODO: Just updating version and environment for now
	repo, ok := platormRepos[eConfig.Platform]
	if !ok {
		logger.Sugar().Infof("Unknown platform\n")
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	sts.Spec.Template.Spec.Containers[0].Image = "dockerhub.cisco.com/cspg-docker/andromeda/" + repo + ":" + eConfig.Version

	envs := []apiv1.EnvVar{
		{
			Name:  "EMU_CLOUD",
			Value: eConfig.Cloud,
		},
		{
			Name: "KUBE_POD_NAME",
			ValueFrom: &apiv1.EnvVarSource{
				FieldRef: &apiv1.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "metadata.name",
				},
			},
		},
	}

	if eConfig.DeviceConfig != nil {
		if repo != "apollo-test" {
			http.Error(w, "DeviceConfig only supported for dc spewer", http.StatusBadRequest)
			return
		}
		b, jerr := json.Marshal(eConfig.DeviceConfig)
		if jerr != nil {
			http.Error(w, "failed to marshal device config : "+jerr.Error(), http.StatusInternalServerError)
			return
		}
		envs = append(envs, apiv1.EnvVar{
			Name:  "DEVICE_CONFIG",
			Value: string(b),
		})
	}

	for k, v := range eConfig.Options {
		envs = append(envs, apiv1.EnvVar{
			Name:  k,
			Value: v,
		})
	}

	sts.Spec.Template.Spec.Containers[0].Env = envs

	_, err = cs.AppsV1().StatefulSets(ns).Update(sts)
	if err != nil {
		logger.Sugar().Infof("sts update err : %s\n", err.Error())
		http.Error(w,
			fmt.Sprintf("sts update err : %s\n", err.Error()),
			http.StatusInternalServerError)
		return
	}
}

func getCertificates(w http.ResponseWriter, r *http.Request) {
	v := mux.Vars(r)

	_, ok := v["id"]
	if !ok {
		logger.Info("No id in request")
		w.WriteHeader(http.StatusBadRequest)
		return
	}

}

func addCertificate(w http.ResponseWriter, r *http.Request) {
	cert, err := ioutil.ReadAll(r.Body)
	if err != nil {
		logger.Info("Failed to read request body", zap.Error(err))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	certId := newRandStr(8)

	certLock.Lock()
	defer certLock.Unlock()
	certificates[certId] = string(cert)

	_, err = w.Write([]byte(certId))
	if err != nil {
		logger.Error("failed to write body", zap.Error(err))
	}
}

func newRandStr(l int) string {
	rs := make([]byte, l)
	n, err := io.ReadFull(rand.Reader, rs)
	if n != len(rs) || err != nil {
		// can't do much without reading from 'rand'
		panic(err)
	}
	return fmt.Sprintf("%x", rs)
}
