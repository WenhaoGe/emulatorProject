package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path"
	"strconv"
	"time"

	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adio"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adlog"
	"github.com/boltdb/bolt"
	"github.com/gorilla/mux"
	"go.uber.org/zap"
)

const (
	podNameHeader       = "x-pod-name"
	devicesBucketPrefix = "devices-"
)

type spewerServer struct {
	logger     *zap.Logger
	ctx        context.Context
	db         *bolt.DB
	storageDir string
}

func (s *spewerServer) initDb(ctx context.Context) {
	logger := adlog.MustFromContext(ctx)
	s.logger = logger
	s.ctx = ctx

	opts := &bolt.Options{
		Timeout: 10 * time.Second,
	}

	var err error
	s.db, err = bolt.Open(path.Join(s.storageDir, "bolt.db"), 0600, opts)
	if err != nil {
		logger.Fatal("failed to open bolt db", zap.Error(err))
	}
}

func (s *spewerServer) getDevice(ctx context.Context, podName, deviceId string) (*EmulatorDevice, error) {
	//logger := adlog.MustFromContext(ctx)
	tx, err := s.db.Begin(false)
	if err != nil {
		return nil, err
	}

	defer func() {
		rerr := tx.Rollback()
		if rerr != nil {
			adlog.MustFromContext(ctx).Error("failed rollback", zap.Error(rerr))
		}
	}()

	bucket := tx.Bucket([]byte(devicesBucketPrefix + podName))
	if bucket == nil {
		return nil, fmt.Errorf("bucket not found")
	}

	deviceb := bucket.Get([]byte(deviceId))

	if deviceb == nil {
		return nil, fmt.Errorf("device not found")
	}

	var outDev EmulatorDevice
	err = adio.JsonToObjectCtx(ctx, bytes.NewReader(deviceb), &outDev)
	return &outDev, err
}

func (s *spewerServer) getDevices(w http.ResponseWriter, r *http.Request) {
	logger := s.logger
	podName := r.Header.Get(podNameHeader)
	s.logger.Sugar().Infof("got request for pod %s", podNameHeader)
	if podName == "" {
		http.Error(w,
			"no pod name in request",
			http.StatusBadRequest)
		return
	}

	var devices []json.RawMessage
	tx, err := s.db.Begin(false)
	if err != nil {
		s.logger.Error("failed to start transaction", zap.Error(err))
		http.Error(w,
			"db error : "+err.Error(),
			http.StatusInternalServerError)
		return
	}

	defer func() {
		rerr := tx.Rollback()
		if rerr != nil {
			fmt.Printf("failed rollback: %ss\n", rerr.Error())
		}
	}()

	bucket := tx.Bucket([]byte(devicesBucketPrefix + podName))
	if bucket != nil {
		c := bucket.Cursor()

		for k, v := c.First(); k != nil; k, v = c.Next() {
			devices = append(devices, v)
		}
	}

	_, err = w.Write(adio.ObjectToJson(context.TODO(), devices, nil))
	if err != nil {
		logger.Error("failed to write response", zap.Error(err))
	}
}

func (s *spewerServer) writeDevice(w http.ResponseWriter, r *http.Request) {
	logger := s.logger
	podName := r.Header.Get(podNameHeader)
	logger.Sugar().Infof("got request to write device for pod %s", podNameHeader)
	if podName == "" {
		http.Error(w,
			"no pod name in request",
			http.StatusBadRequest)
		return
	}

	// Read device config from body
	var device EmulatorDevice
	err := adio.JsonToObjectCtx(s.ctx, r.Body, &device)
	if err != nil {
		http.Error(w,
			"failed to unmarshal device : "+err.Error(),
			http.StatusBadRequest)
		return
	}

	tx, err := s.db.Begin(true)
	if err != nil {
		s.logger.Error("failed to start transaction", zap.Error(err))
		http.Error(w,
			"db error : "+err.Error(),
			http.StatusInternalServerError)
		return
	}

	var dberr error
	defer func() {
		if dberr != nil {
			cerr := tx.Rollback()
			if cerr != nil {
				logger.Error("failed transaction rollback", zap.Error(cerr))
			}
			http.Error(w,
				"db error : "+err.Error(),
				http.StatusInternalServerError)
		} else {
			cerr := tx.Commit()
			if cerr != nil {
				logger.Error("failed transaction commit", zap.Error(cerr))
				http.Error(w,
					"db error : "+err.Error(),
					http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
		}
	}()

	var bucket *bolt.Bucket
	bucket, dberr = tx.CreateBucketIfNotExists([]byte(devicesBucketPrefix + podName))
	if dberr != nil {
		return
	}

	deviceb := adio.ObjectToJson(s.ctx, device, adio.JsonOptSerializeAllNoIndent)
	logger.Sugar().Infof("writing device for %s as %s", podName, deviceb)
	dberr = bucket.Put([]byte(device.AccessKeyId), deviceb)
}

func StartServer() {
	err := flag.Set("log_path", "")
	if err != nil {
		panic(err)
	}

	logger := adlog.InitSimpleLogger(10 << 10 << 10)

	ctx := context.Background()
	ctx = adlog.ContextWithValue(ctx, logger)

	server := &spewerServer{}
	if os.Getenv("STORAGE_DIR") != "" {
		server.storageDir = os.Getenv("STORAGE_DIR")
	} else {
		server.storageDir = "/storage"
	}

	server.initDb(ctx)

	r := mux.NewRouter()

	r.HandleFunc("/devices", server.writeDevice).Methods(http.MethodPost)
	r.HandleFunc("/devices", server.getDevices).Methods(http.MethodGet)

	r.HandleFunc("/inventory", server.importInventory).Methods(http.MethodPost)
	r.HandleFunc("/inventory", server.getInventory).Methods(http.MethodGet)

	// Handles devices writing to their inventory
	r.HandleFunc("/deviceInventory", server.writeDeviceInventory).Methods(http.MethodPost)

	http.Handle("/", r)

	sport := 80
	if os.Getenv("STORAGE_PORT") != "" {
		sport, err = strconv.Atoi(os.Getenv("STORAGE_PORT"))
		if err != nil {
			panic(err)
		}
	}
	logger.Fatal("storage server stopped", zap.Error(http.ListenAndServe(":"+strconv.Itoa(sport), nil)))

}
