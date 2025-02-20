package storage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"strings"

	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adlog"
	"github.com/boltdb/bolt"
	"go.uber.org/zap"
)

const (
	deviceIdQuery         = "deviceId"
	inventoryRequestQuery = "inventoryReq"
	nodeIdQuery           = "nodeId"

	importPlatformHeader = "inventoryPlatform"
	importDbNameHeader   = "inventoryDbName"
)

func (s *spewerServer) importInventory(w http.ResponseWriter, r *http.Request) {
	logger := adlog.MustFromContext(s.ctx)
	logger.Info("in inventory import")

	platform := r.Header.Get(importPlatformHeader)

	logger.Sugar().Infof("got inventory import for platform : ", platform)
	if platform == "" {
		http.Error(w,
			"no platform in request",
			http.StatusBadRequest)
		return
	}

	dbName := r.Header.Get(importDbNameHeader)

	if dbName == "" {
		http.Error(w,
			"no db name in request",
			http.StatusBadRequest)
		return
	}

	logger.Sugar().Infof("writing to db bucket %s", dbName)

	tmpDbFile, err := ioutil.TempFile(os.TempDir(), "invdb")
	if err != nil {
		http.Error(w,
			"error writing to db : "+err.Error(),
			http.StatusInternalServerError)
		return
	}

	defer func() {
		rerr := os.Remove(tmpDbFile.Name())
		if rerr != nil {
			logger.Error("failed to remove temp db file", zap.Error(rerr))
		}
	}()

	_, err = io.Copy(tmpDbFile, r.Body)
	if err != nil {
		cerr := tmpDbFile.Close()
		if cerr != nil {
			logger.Error("failed to close temp db file", zap.Error(err))
		}
		http.Error(w,
			"error writing to db : "+err.Error(),
			http.StatusInternalServerError)
		return
	}

	cerr := tmpDbFile.Close()
	if cerr != nil {
		logger.Error("failed to close temp db file", zap.Error(err))
		http.Error(w,
			"error writing to db : "+err.Error(),
			http.StatusInternalServerError)
		return
	}

	tmpDb, err := bolt.Open(tmpDbFile.Name(), 0666, &bolt.Options{ReadOnly: true})
	if err != nil {
		http.Error(w,
			"error reading input db : "+err.Error(),
			http.StatusInternalServerError)
		return
	}

	defer func() {
		berr := tmpDb.Close()
		if berr != nil {
			logger.Error("failed to close temp db", zap.Error(berr))
		}
	}()

	err = s.db.Update(func(tx *bolt.Tx) error {
		pbucket, dberr := tx.CreateBucketIfNotExists([]byte(platform))
		if dberr != nil {
			return dberr
		}

		invDbBucket, dberr := pbucket.CreateBucketIfNotExists([]byte(dbName))
		if dberr != nil {
			return dberr
		}

		dberr = tmpDb.View(func(temptx *bolt.Tx) error {
			return temptx.ForEach(func(name []byte, b *bolt.Bucket) error {
				var subBucket *bolt.Bucket
				if platform == "HX" {
					subBucket = invDbBucket
				} else {
					subBucket, err = invDbBucket.CreateBucketIfNotExists(name)
					if err != nil {
						return err
					}
				}
				c := b.Cursor()

				logger.Sugar().Infof("writing to sub bucket: %s", string(name))

				for k, v := c.First(); k != nil; k, v = c.Next() {
					perr := subBucket.Put(k, bytes.Trim(v, "\x00"))
					if perr != nil {
						return perr
					}
				}

				return nil
			})
		})
		if dberr != nil {
			return dberr
		}

		return nil
	})
	if err != nil {
		http.Error(w,
			"error writing to db : "+err.Error(),
			http.StatusInternalServerError)
	}
}

func (s *spewerServer) parseRequest(w http.ResponseWriter, r *http.Request) (*EmulatorDevice, string, string, error) {
	podName := r.Header.Get(podNameHeader)
	s.logger.Sugar().Debugf("got inventory request for pod %s", podName)
	if podName == "" {
		return nil, "", "", fmt.Errorf("no pod name in request")
	}

	qp := r.URL.Query()

	deviceid := qp.Get(deviceIdQuery)
	if deviceid == "" {
		return nil, "", "", fmt.Errorf("no device name in request")
	}

	inventoryReq := qp.Get(inventoryRequestQuery)
	if inventoryReq == "" {
		return nil, "", "", fmt.Errorf("no inventory request in request")
	}

	nodeId := qp.Get(nodeIdQuery)

	device, err := s.getDevice(s.ctx, podName, deviceid)
	if err != nil {
		return nil, "", "", fmt.Errorf("failed to find device : " + err.Error())
	}
	return device, nodeId, inventoryReq, nil
}

func (s *spewerServer) getInventory(w http.ResponseWriter, r *http.Request) {
	logger := adlog.MustFromContext(s.ctx)

	device, nodeId, inventoryReq, err := s.parseRequest(w, r)
	if err != nil {
		http.Error(w,
			err.Error(),
			http.StatusBadRequest)
		return
	}

	logger.Sugar().Infof("getting inventory for device %s of type %s", device.Moid, device.Config.PlatformType)

	switch device.Config.PlatformType {
	case "HX":
		s.hxInventory(w, device, nodeId, inventoryReq)
		return
	}

	s.defaultInventory(w, device, nodeId, inventoryReq)
}

func (s *spewerServer) writeDeviceInventory(w http.ResponseWriter, r *http.Request) {
	device, nodeId, inventoryReq, err := s.parseRequest(w, r)
	if err != nil {
		http.Error(w,
			err.Error(),
			http.StatusBadRequest)
		return
	}

	uperr := s.db.Update(func(tx *bolt.Tx) error {
		platbucket := tx.Bucket([]byte(device.Config.PlatformType))
		if platbucket == nil {
			return fmt.Errorf("%ss bucket doesn't exist", device.Config.PlatformType)
		}

		deviceInvBucket, err := platbucket.CreateBucketIfNotExists([]byte(device.AccessKeyId))
		if err != nil {
			return err
		}

		invBucket, err := deviceInvBucket.CreateBucketIfNotExists([]byte(nodeId))
		if err != nil {
			return err
		}

		buf := make([]byte, r.ContentLength)
		_, err = io.ReadFull(r.Body, buf)
		if err != nil {
			return err
		}

		return invBucket.Put([]byte(inventoryReq), buf)
	})

	if uperr != nil {
		http.Error(
			w,
			"failed to write inventory : "+uperr.Error(),
			http.StatusInternalServerError,
		)
	}

}

func (s *spewerServer) hxInventory(w http.ResponseWriter, device *EmulatorDevice, nodeId, inventoryReq string) {
	logger := adlog.MustFromContext(s.ctx)
	logger.Sugar().Debugf("hx request for %s", inventoryReq)

	dbName := device.Config.InventoryDatabase
	if dbName == "" {
		dbName = "default"
	}

	err := s.db.View(func(tx *bolt.Tx) error {
		hxinv := tx.Bucket([]byte("HX"))
		if hxinv == nil {
			return fmt.Errorf("hx bucket doesn't exist")
		}

		// First check if device has written back to the inventory
		deviceConf := hxinv.Bucket([]byte(device.AccessKeyId))
		if deviceConf != nil {
			nodeConf := deviceConf.Bucket([]byte(nodeId))
			if nodeConf != nil {
				confinv := nodeConf.Get([]byte(inventoryReq))
				if len(confinv) > 0 {
					_, oerr := w.Write(confinv)
					return oerr
				}
			}
		}

		invDb := hxinv.Bucket([]byte(dbName))
		if invDb == nil {
			return fmt.Errorf("%s db does not exist", dbName)
		}

		key := []byte(inventoryReq)
		// Get the overall object first if it's not a request for overall itself
		if inventoryReq != "overall" {
			overall := invDb.Get([]byte("overall"))
			if overall == nil {
				return fmt.Errorf("no overall found for device")
			}

			var overallm map[string]interface{}
			jerr := json.Unmarshal(overall, &overallm)
			if jerr != nil {
				return jerr
			}

			if hyperv, ok := overallm["hypervisor"].(string); ok && hyperv != "" &&
				inventoryReq == "appliances" {
				key = []byte("nodes")
			}

			if uuid, ok := overallm["cluster_uuid"].(string); ok {
				key = []byte(strings.Replace(string(key), uuid, "{cuuid}", -1))
			}
		}

		output := invDb.Get(key)
		if output == nil {
			return fmt.Errorf("%s not found", inventoryReq)
		}

		_, oerr := w.Write(output)
		return oerr
	})

	if err != nil {
		http.Error(
			w,
			"failed to query for inventory : "+err.Error(),
			http.StatusInternalServerError,
		)
	}
}

func (s *spewerServer) defaultInventory(w http.ResponseWriter, device *EmulatorDevice, nodeId, inventoryReq string) {
	logger := adlog.MustFromContext(s.ctx)
	logger.Sugar().Debugf("platform %s inventory request for %s", device.Config.PlatformType, inventoryReq)

	dbName := device.Config.InventoryDatabase
	if dbName == "" {
		dbName = "default"
	}

	err := s.db.View(func(tx *bolt.Tx) error {
		platInv := tx.Bucket([]byte(device.Config.PlatformType))
		if platInv == nil {
			return fmt.Errorf("%s bucket doesn't exist", device.Config.PlatformType)
		}

		// First check if device has written back to the inventory
		deviceConf := platInv.Bucket([]byte(device.AccessKeyId))
		if deviceConf != nil {
			nodeConf := deviceConf.Bucket([]byte(nodeId))
			if nodeConf != nil {
				confinv := nodeConf.Get([]byte(inventoryReq))
				if len(confinv) > 0 {
					_, oerr := w.Write(confinv)
					return oerr
				}
			}
		}

		invDb := platInv.Bucket([]byte(dbName))
		if invDb == nil {
			return fmt.Errorf("%s db does not exist", dbName)
		}

		// Get node inventory bucket
		nodeInv := invDb.Bucket([]byte(nodeId))
		if nodeInv == nil {
			return fmt.Errorf("%s node inventory does not exist", nodeId)
		}

		inv := nodeInv.Get([]byte(inventoryReq))

		_, oerr := w.Write(inv)
		return oerr
	})

	if err != nil {
		http.Error(
			w,
			"failed to query for inventory : "+err.Error(),
			http.StatusInternalServerError,
		)
	}
}
