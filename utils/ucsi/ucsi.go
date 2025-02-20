package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"bitbucket-eng-sjc1.cisco.com/an/apollo-test/dcSpewer/utils"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adio"
	"github.com/boltdb/bolt"
)

type DCRequestTrace struct {
	ObjectType string
	ActionType string
	MessageId  string
	MoList     []interface{}
}

// Describes the data that is returned from Device Connector to Intersight
type DCResponseTrace struct {
	ObjectType string
	ActionType string
	ErrorText  string
	MessageId  string
	MoList     []interface{}
	ResultCode string
}

// Converts debug trace files taken from a UCSFIISM cluster and creates an inventory database that
// can be imported into the emulator storage server and sourced for ISM inventory
//
// Usage: ./ucsfi <fi_a_trace> <fi_b_trace>
func main() {
	inventory := make(map[string]interface{})
	parseResponseFile("A", inventory)
	parseResponseFile("B", inventory)

	//fmt.Printf("out inv : %s\n", adio.ObjectToJson(context.Background(), inventory, nil))

	db, err := bolt.Open("./ucsi.db", 0666, &bolt.Options{})
	if err != nil {
		panic(err)
	}

	err = db.Update(func(tx *bolt.Tx) error {
		// Create bucket for each node
		for node, inv := range inventory {
			nodebucket, err := tx.CreateBucketIfNotExists([]byte(node))
			if err != nil {
				return err
			}

			// Create a key for each api request
			for req, resp := range inv.(map[string]interface{}) {
				key := []byte(req)
				val := adio.ObjectToJson(context.Background(), resp, adio.JsonOptSerializeAllNoIndent)
				fmt.Printf("writing key %s with val : \n %s \n", key, val)
				err = nodebucket.Put(key,
					val)
				if err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		panic(err)
	}
}

func parseResponseFile(node string, inventory map[string]interface{}) {
	var filename string
	if node == "A" {
		filename = os.Args[1]
	} else {
		filename = os.Args[2]
	}
	f, err := os.Open(filename)
	if err != nil {
		panic(err)
	}

	defer func() {
		if cerr := f.Close(); cerr != nil {
			panic(cerr)
		}
	}()

	var traceResponses []utils.DebugTraceMessage
	err = adio.JsonToObjectCtx(context.Background(), f, &traceResponses)
	if err != nil {
		panic(err)
	}

	// Going through each response to analyze if inventory is being returned
	nodeInv := make(map[string]interface{})

	for _, req := range traceResponses {
		if req.Type == "TypeNetworkAgent" {
			var reqTrace DCRequestTrace
			var val DCResponseTrace
			if err := json.Unmarshal(req.Response, &val); err != nil {
				panic(err)
			}
			if err := json.Unmarshal(req.Request, &reqTrace); err != nil {
				panic(err)
			}
			nodeInv[reqTrace.ActionType] = val.MoList
		}
	}
	inventory[node] = nodeInv
}
