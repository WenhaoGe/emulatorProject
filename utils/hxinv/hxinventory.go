package main

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strings"

	"github.com/boltdb/bolt"
)

const (
	// Constants used for Header
	ClusterUuidString     = "cluster_uuid"
	ClusterNameString     = "cluster_name"
	ClusterHypervisor     = "hypervisor"
	ClusterProductVersion = "productVersion"
)

func getClusterHeader(overall map[string]interface{}) map[string]interface{} {
	// Header for the API response.
	// Currently it will have cluster_uuid, cluster_name, hypervisor, productVersion.
	out := make(map[string]interface{})
	out[ClusterUuidString] = overall["overview"].(map[string]interface{})["config"].(map[string]interface{})["clusterUuid"]
	out[ClusterNameString] = overall["overview"].(map[string]interface{})["config"].(map[string]interface{})["name"]
	out[ClusterHypervisor] = overall["overview"].(map[string]interface{})["about"].(map[string]interface{})["hypervisor"]
	out[ClusterProductVersion] = overall["overview"].(map[string]interface{})["about"].(map[string]interface{})["productVersion"]
	return out
}

// Run on hx platform root shell, collects inventory from HXDP and saves it to a bolt database file
// that can be imported into the emulator farm.
func main() {
	tlsconf := &tls.Config{InsecureSkipVerify: true} // nolint
	transp := &http.Transport{TLSClientConfig: tlsconf}
	c := &http.Client{Transport: transp}

	// Get the root session token
	pubfile, err := os.Open("/etc/springpath/secure/root_file.pub")
	if err != nil {
		panic(err)
	}

	// Read lines from the file to extract the key.
	var lines []string
	lScanner := bufio.NewScanner(pubfile)
	for lScanner.Scan() {
		lines = append(lines, lScanner.Text())
	}

	// Close the file.
	err = pubfile.Close()
	if (err != nil) || (len(lines) == 0) {
		panic("RootSessionId not found or cannot close. Cannot access REST API")
	}

	sessionId := lines[0]

	db, err := bolt.Open("/tmp/hxinv.db", 0666, &bolt.Options{})
	if err != nil {
		panic(err)
	}

	// Get overall first
	var overall map[string]interface{}
	ovout := executeApi(c, "hx/api/clusters/1/overall", sessionId)
	if ovout == nil {
		ovout = executeApi(c, "rest/clusters", sessionId)
		fmt.Printf("got clusters : %s\n", string(ovout))
		var overview []map[string]interface{}
		err = json.Unmarshal(ovout, &overview)
		if err != nil {
			panic(err)
		}
		overall = map[string]interface{}{"overview": overview[0]}
	} else {
		fmt.Printf("got overall : %s\n", string(ovout))
		err = json.Unmarshal(ovout, &overall)
		if err != nil {
			panic(err)
		}
	}
	ovHeader := getClusterHeader(overall)

	// Get UUID for 3.0 clusters
	extraout := executeApi(c, "coreapi/v1/clusters", sessionId)
	if extraout != nil {
		var extra []map[string]interface{}
		err = json.Unmarshal(extraout, &extra)
		if err != nil {
			panic(err)
		}

		fmt.Printf("got extra : %+v", extra)
		uuid := extra[0]["uuid"]
		ovHeader[ClusterUuidString] = uuid
	}

	fmt.Printf("clust header : %+v\n", ovHeader)
	err = db.Update(func(tx *bolt.Tx) error {
		b, dberr := tx.CreateBucketIfNotExists([]byte("inventory"))
		if dberr != nil {
			return dberr
		}

		invbits, dberr := json.Marshal(ovHeader)
		if dberr != nil {
			return dberr
		}
		dberr = b.Put([]byte("overall"), invbits)
		if dberr != nil {
			return dberr
		}
		return nil
	})
	if err != nil {
		panic(err)
	}

	apis := map[string]string{
		"nodes":                   "hx/api/clusters/1/nodes",
		"summary":                 "rest/summary",
		"virtplatform/alarms":     "rest/virtplatform/alarms",
		"hypervisor/hosts":        "coreapi/v1/hypervisor/hosts",
		"appliances":              "rest/appliances",
		"clusters/{cuuid}/detail": "coreapi/v1/clusters/{cuuid}/detail",
		"clusters/{cuuid}/health": "coreapi/v1/clusters/{cuuid}/health",
		"clusters/{cuuid}/alarms": "coreapi/v1/clusters/{cuuid}/alarms",
	}

	for name, api := range apis {
		out := executeApi(c, strings.Replace(api, "{cuuid}", ovHeader[ClusterUuidString].(string), -1), sessionId)

		if out == nil {
			continue
		}

		apiOut := make(map[string]interface{})
		for k, v := range ovHeader {
			apiOut[k] = v
		}

		apiOut[name] = json.RawMessage(out)

		jApiOut, err := json.Marshal(apiOut)
		if err != nil {
			panic(err)
		}
		fmt.Printf("got out : %s\n", jApiOut)
		err = db.Update(func(tx *bolt.Tx) error {
			b, dberr := tx.CreateBucketIfNotExists([]byte("inventory"))
			if dberr != nil {
				return dberr
			}

			dberr = b.Put([]byte(name), jApiOut)
			if dberr != nil {
				return dberr
			}
			return nil
		})
		if err != nil {
			panic(err)
		}
	}
}

const (
	HeaderRequestInitiatorIpAddress = "127.0.0.1"
	HeaderRequestUsername           = "hxdc"
	HeaderLoggedUser                = "X-LoggedInUser"
	HeaderRequestInitiator          = "X-RequestInitiator"
	HeaderScope                     = "X-Scope"
	HeaderRootSessionId             = "X-RootSessionID"
	HeaderAcceptString              = "Accept"
	HeaderRequestType               = "application/json"
	HeaderReadScope                 = "READ"
)

func executeApi(c *http.Client, api string, sesssionId string) []byte {
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("https://localhost/%s", api), nil)
	if err != nil {
		panic(err)
	}
	req.Header.Set(HeaderAcceptString, HeaderRequestType)
	req.Header.Set(HeaderRootSessionId, sesssionId)
	req.Header.Set(HeaderScope, HeaderReadScope)
	req.Header.Set(HeaderRequestInitiator, HeaderRequestInitiatorIpAddress)
	req.Header.Set(HeaderLoggedUser, HeaderRequestUsername)

	resp, err := c.Do(req)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Printf("unexpected status code in response for api %s : %d\n", api, resp.StatusCode)
		return nil
	}

	//buf := make([]byte, resp.ContentLength)
	buf, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		panic(err)
	}
	return buf
}
