package apiServer

import (
	"fmt"
	"net/http"

	"context"

	"net/http/pprof"
	"runtime"

	"bitbucket-eng-sjc1.cisco.com/an/apollo-test/dcSpewer/emulatorConnection"
	"bitbucket-eng-sjc1.cisco.com/an/apollo-test/dcSpewer/utils"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adio"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adio/json"
	"github.com/gorilla/mux"
)

func getEmulatorState(w http.ResponseWriter, r *http.Request) {
	var states []emulatorConnection.EmulatorState
	if utils.GetErrorState() != "" {
		states = []emulatorConnection.EmulatorState{{ErrorState: utils.GetErrorState()}}
	} else {
		states = emulatorConnection.GetEmulatorConnectionStatus()
	}

	enc := json.NewEncoder(w)

	err := enc.Encode(&states)
	if err != nil {
		fmt.Println("failed to write emulator states: ", err.Error())
	}
}

func getEmulatorId(w http.ResponseWriter, r *http.Request) {
	ids := emulatorConnection.GetEmulatorIds()

	enc := json.NewEncoder(w)

	err := enc.Encode(&ids)
	if err != nil {
		fmt.Println("failed to write emulator states: ", err.Error())
	}
}

func getEmulatorVersion(w http.ResponseWriter, r *http.Request) {
	_, err := w.Write([]byte("notreal"))
	if err != nil {
		fmt.Println("failed to write body : ", err.Error())
	}
}

func getEmulatorToken(w http.ResponseWriter, r *http.Request) {
	tokens := emulatorConnection.GetEmulatorTokens()
	enc := json.NewEncoder(w)

	err := enc.Encode(&tokens)
	if err != nil {
		fmt.Println("failed to write emulator tokens: ", err.Error())
	}
}

func configInventory(w http.ResponseWriter, r *http.Request) {
	v := mux.Vars(r)

	id, ok := v["Identity"]
	if !ok {
		http.Error(w, "No identity given", http.StatusBadRequest)
		return
	}

	err := emulatorConnection.ImportInventoryConfig(id, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
}

func deviceApi(w http.ResponseWriter, r *http.Request) {
	v := mux.Vars(r)

	id, ok := v["Identity"]
	if !ok {
		http.Error(w, "No identity given", http.StatusBadRequest)
		return
	}

	api, ok := v["Api"]
	if !ok {
		http.Error(w, "No api given", http.StatusBadRequest)
		return
	}

	emulator := emulatorConnection.GetEmulator(id)
	if emulator == nil {
		http.Error(w, fmt.Sprintf("%s not found", id), http.StatusNotFound)
		return
	}

	switch api {
	case "SecurityTokens":
		token, devid, err := emulator.GetSecurityToken()
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to get security token: %s", err.Error()), http.StatusInternalServerError)
			return
		}

		_, err = w.Write(adio.ObjectToJson(context.TODO(), map[string]interface{}{
			"DeviceIdentifier": devid,
			"Token":            token,
		}, nil))
		if err != nil {
			fmt.Println("response write error: ", err.Error())
		}
	}
}

func deviceState(w http.ResponseWriter, r *http.Request) {
	v := mux.Vars(r)

	id, ok := v["Identity"]
	if !ok {
		http.Error(w, "No identity given", http.StatusBadRequest)
		return
	}

	emulator := emulatorConnection.GetEmulator(id)
	if emulator == nil {
		http.Error(w, fmt.Sprintf("%s not found", id), http.StatusNotFound)
		return
	}

	state := emulator.GetConnectionStatuses()

	out := emulatorConnection.EmulatorToken{
		EmulatorState: emulatorConnection.EmulatorState{
			Id:                 id,
			Account:            emulator.GetAccount(),
			ConnectionStatuses: state,
		},
	}

	if emulator.GetAccount() == "" {
		token, devid, err := emulator.GetSecurityToken()
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to get security token: %s", err.Error()), http.StatusInternalServerError)
			return
		}
		out.Token = token
		out.DeviceIdentifier = devid
	}

	_, err := w.Write(adio.ObjectToJson(context.TODO(), out, nil))
	if err != nil {
		fmt.Println("response write error: ", err.Error())
	}
}

func toggleAllDevices(idchange bool) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		err := emulatorConnection.ToggleConnections(idchange)
		if err != nil {
			http.Error(w,
				err.Error(),
				http.StatusInternalServerError)
		}
	}
}

func getStats(w http.ResponseWriter, r *http.Request) {
	out := make(map[string]interface{})
	var memstats runtime.MemStats
	runtime.ReadMemStats(&memstats)
	out["mem"] = memstats
	out["routinecount"] = runtime.NumGoroutine()

	_, err := w.Write(adio.ObjectToJson(context.TODO(), out, nil))
	if err != nil {
		fmt.Println("response write error: ", err.Error())
	}
}

func Start() {

	r := mux.NewRouter()

	r.HandleFunc("/Systems", getEmulatorState)
	r.HandleFunc("/GetIdentity", getEmulatorId)
	r.HandleFunc("/Versions", getEmulatorVersion)

	r.HandleFunc("/DeviceIdentifiers", getEmulatorToken)

	r.HandleFunc("/SecurityTokens", getEmulatorToken)

	r.HandleFunc("/toggleids", toggleAllDevices(true))
	r.HandleFunc("/toggle", toggleAllDevices(false))

	// Debug apis for collecting pprof profiles from process.
	r.HandleFunc("/debug/stats", getStats)
	r.HandleFunc("/debug/pprof/cmdline", http.HandlerFunc(pprof.Cmdline))
	r.HandleFunc("/debug/pprof/profile", http.HandlerFunc(pprof.Profile))
	r.HandleFunc("/debug/pprof/symbol", http.HandlerFunc(pprof.Symbol))
	r.HandleFunc("/debug/pprof/trace", http.HandlerFunc(pprof.Trace))
	r.NewRoute().PathPrefix("/debug/pprof").HandlerFunc(http.HandlerFunc(pprof.Index))

	r.HandleFunc("/Inventory/{Identity}", configInventory).Methods(http.MethodPost)

	r.HandleFunc("/{Identity}/{Api}", deviceApi)

	r.HandleFunc("/{Identity}", deviceState)

	http.Handle("/", r)

	go func() {
		err := http.ListenAndServe(":80", nil)
		if err != nil {
			panic("ListenAndServe: " + err.Error())
		}
	}()

}
