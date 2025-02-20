package storage

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"sync"

	"bitbucket-eng-sjc1.cisco.com/an/apollo-test/dcSpewer/utils"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adio"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adlog"
	"github.com/boltdb/bolt"
	"go.uber.org/zap"
)

type EmulatorStorage interface {
	GetAllDevices(ctx context.Context) []*EmulatorDevice

	WriteDevice(ctx context.Context, device *EmulatorDevice) error

	GetInventory(ctx context.Context, inventoryReq string) (*bytes.Buffer, error)

	WriteInventory(ctx context.Context, inventoryReq string, buf *bytes.Buffer) error
}

type localStorage struct {
	db *bolt.DB
	mu sync.Mutex
}

// Bolt db file name to store connection credentials.
const emuDbFile = "emuDb.db"

// Bucket in bolt db file to store connection credentials.
const emuBucketName = "EmuBucket"

func (ls *localStorage) init(ctx context.Context) {
	logger := adlog.MustFromContext(ctx)

	var err error
	ls.db, err = bolt.Open(emuDbFile, 0600, nil)
	if err != nil {
		logger.Fatal("failed to open db file", zap.Error(err))
	}
}

func (ls *localStorage) GetAllDevices(ctx context.Context) []*EmulatorDevice {
	logger := adlog.MustFromContext(ctx)
	ls.mu.Lock()
	if ls.db == nil {
		ls.init(ctx)
	}
	ls.mu.Unlock()

	var outDevices []*EmulatorDevice

	err := ls.db.View(func(tx *bolt.Tx) error {
		// Assume bucket exists and has keys
		b := tx.Bucket([]byte(emuBucketName))
		if b == nil {
			logger.Error("bucket doesn't exist")
			return nil
		}

		c := b.Cursor()

		for k, v := c.First(); k != nil; k, v = c.Next() {
			emuC := &EmulatorDevice{}

			uerr := json.Unmarshal(v, emuC)
			if uerr != nil {
				logger.Error("failed to parse emu config", zap.Error(uerr))
				continue
			}

			var privateKey *rsa.PrivateKey
			block, _ := pem.Decode([]byte(emuC.PrivateKeyPem))

			if block == nil {
				logger.Error("failed to parse PEM block containing the key")
				continue
			}

			var perr error
			privateKey, perr = x509.ParsePKCS1PrivateKey(block.Bytes)
			if perr != nil {
				logger.Error("Failed to parse private key", zap.Error(perr))
				continue
			}
			emuC.PrivateKey = privateKey

			outDevices = append(outDevices, emuC)
		}

		return nil
	})

	if err != nil {
		logger.Error("failed to get existing devices from local db", zap.Error(err))
	}
	return outDevices
}

func (ls *localStorage) WriteDevice(ctx context.Context, device *EmulatorDevice) error {
	logger := adlog.MustFromContext(ctx)
	ls.mu.Lock()
	if ls.db == nil {
		ls.init(ctx)
	}
	ls.mu.Unlock()

	return ls.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(emuBucketName))
		if err != nil {
			return err
		}

		emuBits, jerr := json.Marshal(device)
		if jerr != nil {
			logger.Error("failed to marshal emuconfig", zap.Error(jerr))
			return jerr
		}
		return b.Put([]byte(device.AccessKeyId), emuBits)
	})
}

func (ls *localStorage) GetInventory(ctx context.Context, inventoryReq string) (*bytes.Buffer, error) {
	//content := make(map[string]interface{})
	var contents *bytes.Buffer
	trace := utils.GetTraceFromContext(ctx)

	err := ls.db.View(func(tx *bolt.Tx) error {
		// Assume bucket exists and has keys
		b := tx.Bucket([]byte(trace.Node))
		if b == nil {
			return fmt.Errorf("bucket A doesn't exist")
		}

		val := b.Get([]byte(inventoryReq))
		contents = bytes.NewBuffer(val)

		return nil
	})

	return contents, err
	//return nil, fmt.Errorf("not implemented")
}

func (ls *localStorage) WriteInventory(ctx context.Context, inventoryReq string, buf *bytes.Buffer) error {
	trace := utils.GetTraceFromContext(ctx)
	err := ls.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(trace.Node))
		if err != nil {
			return fmt.Errorf("create bucket: %s", err)
		}

		data, err := ioutil.ReadAll(buf)
		if err != nil {
			return err
		}
		err = b.Put([]byte(inventoryReq), data)

		return err
	})

	return err
}

type farmStorage struct {
	c *http.Client

	storageServer string
}

const farmStorageServer = "http://emulatorstorage.default.svc.cluster.local"

func (fs *farmStorage) GetAllDevices(ctx context.Context) []*EmulatorDevice {
	logger := adlog.MustFromContext(ctx)
	logger.Info("getting devices from farm storage")

	req, err := http.NewRequest(http.MethodGet, fs.storageServer+"/devices", nil)
	if err != nil {
		logger.Error("failed to create request", zap.Error(err))
		return nil
	}
	req.Header.Add(podNameHeader, os.Getenv("KUBE_POD_NAME"))

	resp, err := fs.c.Do(req)
	if err != nil {
		logger.Error("failed to execute device fetch", zap.Error(err))
		return nil
	}

	defer resp.Body.Close() // nolint

	if resp.StatusCode != http.StatusOK {
		logger.Sugar().Errorf("bad response from server : %d", resp.StatusCode)
		return nil
	}

	var devices []*EmulatorDevice
	err = adio.JsonToObjectCtx(ctx, resp.Body, &devices)
	if err != nil {
		logger.Error("failed unmarshal of devices response", zap.Error(err))
	}

	for _, dev := range devices {
		var privateKey *rsa.PrivateKey
		block, _ := pem.Decode([]byte(dev.PrivateKeyPem))

		if block == nil {
			logger.Error("failed to parse PEM block containing the key")
			continue
		}

		var perr error
		privateKey, perr = x509.ParsePKCS1PrivateKey(block.Bytes)
		if perr != nil {
			logger.Error("Failed to parse private key", zap.Error(perr))
			continue
		}
		dev.PrivateKey = privateKey
	}

	return devices
}

func (fs *farmStorage) WriteDevice(ctx context.Context, device *EmulatorDevice) error {
	logger := adlog.MustFromContext(ctx)
	logger.Info("writing device to storage")

	req, err := http.NewRequest(http.MethodPost, fs.storageServer+"/devices",
		bytes.NewReader(adio.ObjectToJson(ctx, device, nil)))
	if err != nil {
		logger.Error("failed to create request", zap.Error(err))
		return nil
	}
	req.Header.Add(podNameHeader, os.Getenv("KUBE_POD_NAME"))

	resp, err := fs.c.Do(req)
	if err != nil {
		logger.Error("failed to execute device write", zap.Error(err))
		return err
	}

	defer resp.Body.Close() // nolint

	if resp.StatusCode != http.StatusOK {
		logger.Sugar().Errorf("bad response from server : %d", resp.StatusCode)
		return fmt.Errorf("bad response from server : %d", resp.StatusCode)
	}

	return nil
}

func (fs *farmStorage) GetInventory(ctx context.Context, inventoryReq string) (*bytes.Buffer, error) {
	logger := adlog.MustFromContext(ctx)

	trace := utils.GetTraceFromContext(ctx)
	logger.Info("requesting inventory from storage server")

	u, err := url.Parse(fs.storageServer + "/inventory")
	if err != nil {
		return nil, err
	}

	q := u.Query()
	q.Set(inventoryRequestQuery, inventoryReq)
	q.Set(deviceIdQuery, trace.Uuid)
	q.Set(nodeIdQuery, trace.Node)
	u.RawQuery = q.Encode()

	logger.Sugar().Infof("sending request for inventory : %s", u.String())
	req, err := http.NewRequest(http.MethodGet, u.String(),
		nil)
	if err != nil {
		logger.Error("failed to create request", zap.Error(err))
		return nil, err
	}
	req.Header.Add(podNameHeader, os.Getenv("KUBE_POD_NAME"))

	resp, err := fs.c.Do(req)
	if err != nil {
		logger.Error("failed to execute device write", zap.Error(err))
		return nil, err
	}

	defer resp.Body.Close() // nolint

	if resp.StatusCode != http.StatusOK {
		errOut, respErr := ioutil.ReadAll(resp.Body)
		if respErr != nil {
			logger.Error("failed to read error body", zap.Error(respErr))
		}
		logger.Sugar().Errorf("bad response from server : %d : %s", resp.StatusCode, string(errOut))
		return nil, fmt.Errorf("bad response from server : %d", resp.StatusCode)
	}

	outbuf := bytes.NewBuffer(nil)
	_, err = io.Copy(outbuf, resp.Body)
	return outbuf, err
}

func (fs *farmStorage) WriteInventory(ctx context.Context, inventoryReq string, buf *bytes.Buffer) error {
	logger := adlog.MustFromContext(ctx)

	trace := utils.GetTraceFromContext(ctx)
	logger.Info("sending inventory to storage server")

	u, err := url.Parse(fs.storageServer + "/deviceInventory")
	if err != nil {
		return err
	}

	q := u.Query()
	q.Set(inventoryRequestQuery, inventoryReq)
	q.Set(deviceIdQuery, trace.Uuid)
	q.Set(nodeIdQuery, trace.Node)
	u.RawQuery = q.Encode()
	logger.Sugar().Infof("sending write of inventory : %s", u.String())
	req, err := http.NewRequest(http.MethodPost, u.String(),
		buf)
	if err != nil {
		logger.Error("failed to create request", zap.Error(err))
		return err
	}
	req.Header.Add(podNameHeader, os.Getenv("KUBE_POD_NAME"))

	resp, err := fs.c.Do(req)
	if err != nil {
		logger.Error("failed to execute device write", zap.Error(err))
		return err
	}

	defer resp.Body.Close() // nolint

	if resp.StatusCode != http.StatusOK {
		errOut, respErr := ioutil.ReadAll(resp.Body)
		if respErr != nil {
			logger.Error("failed to read error body", zap.Error(respErr))
		}
		logger.Sugar().Errorf("bad response from server : %d : %s", resp.StatusCode, string(errOut))
		return fmt.Errorf("bad response from server : %d", resp.StatusCode)
	}

	return nil
}

var storageImp EmulatorStorage

func GetEmulatorStorage(ctx context.Context) EmulatorStorage {
	if storageImp == nil {
		// If running under kube, use the farm storage, else use local file storage.
		if os.Getenv("KUBE_POD_NAME") != "" || os.Getenv("STORAGE_SERVER") != "" {
			storageServer := os.Getenv("STORAGE_SERVER")
			if storageServer == "" {
				storageServer = farmStorageServer
			}

			if os.Getenv("KUBE_POD_NAME") == "" {
				os.Setenv("KUBE_POD_NAME", "local")
			}
			storageImp = &farmStorage{
				c:             &http.Client{},
				storageServer: storageServer,
			}

		} else {
			storageImp = &localStorage{}
		}
	}
	return storageImp
}
