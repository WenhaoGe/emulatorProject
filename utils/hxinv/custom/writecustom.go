package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"strings"

	"github.com/boltdb/bolt"
)

// Utility to write custom HX json responses into an existing spewer inventory database
// created with the hxinventory utility.
func main() {
	db, err := bolt.Open(os.Args[1], 0666, &bolt.Options{})
	if err != nil {
		panic(err)
	}

	var overall map[string]interface{}

	err = db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("inventory"))
		ovb := b.Get([]byte("overall"))
		err = json.Unmarshal(ovb, &overall)
		if err != nil {
			panic(err)
		}
		return nil
	})
	if err != nil {
		panic(err)
	}

	fmt.Printf("got overall : %+v\n", overall)

	err = db.Update(func(tx *bolt.Tx) error {
		err = tx.ForEach(func(name []byte, b *bolt.Bucket) error {
			fmt.Println(string(name))
			c := b.Cursor()

			for k, v := c.First(); k != nil; k, v = c.Next() {
				if strings.Contains(string(k), "{cuuid}") {
					err = b.Put(k,
						[]byte(strings.Replace(string(v), "{cuuid}", overall["cluster_uuid"].(string), -1)))
					if err != nil {
						panic(err)
					}
				}
			}
			for k, v := c.First(); k != nil; k, v = c.Next() {
				fmt.Println(string(k))
				fmt.Println(string(v))
			}
			return nil
		})
		return err
	})
	if err != nil {
		panic(err)
	}

	b, err := ioutil.ReadFile(os.Args[3])
	if err != nil {
		panic(err)
	}

	apiOut := make(map[string]interface{})
	for k, v := range overall {
		apiOut[k] = v
	}

	apiOut[os.Args[2]] = json.RawMessage(b)
	jApiOut, err := json.Marshal(apiOut)
	if err != nil {
		panic(err)
	}

	err = db.Update(func(tx *bolt.Tx) error {
		b, dberr := tx.CreateBucketIfNotExists([]byte("inventory"))
		if dberr != nil {
			return dberr
		}

		dberr = b.Put([]byte(os.Args[2]), jApiOut)
		if dberr != nil {
			return dberr
		}
		return nil
	})
	if err != nil {
		panic(err)
	}

}
