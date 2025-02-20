package plugins

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"

	"time"

	"bitbucket-eng-sjc1.cisco.com/an/apollo-test/dcSpewer/utils"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adconnector"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adio"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adlog"
	"github.com/boltdb/bolt"
	"github.com/clbanning/mxj"
	"go.uber.org/zap"
)

// XML plugin reads from a bolt db database containing key/value pairs with key as class name
// and value as xml response from "UCSM"
// TODO: IMC Support
// TODO: Inventory modifications/changes
// TODO: Error inducing
type XmlPlugin struct {
	jobPlugin *JobPlugin
}

type xmlMessage struct {
	XmlRequest string
}

var dellock sync.Mutex

type xmldb struct {
	// Handle to bolt db containing all dns -> xml object
	boltdb *bolt.DB

	// Class -> list of dns, used to do class level querys
	classes map[string][]string

	// Config diffs applied by cloud for each device.
	// DeviceId -> dn -> configs
	configs map[string]map[string]mxj.Map
	// DeviceId -> class -> dns. Used for additions.
	configClassess map[string]map[string][]string

	// Cache of class -> xml payload. Used to optimize initial job runs
	// and avoid reserializing.
	classCache   map[string][]byte
	cacheHitLast time.Time

	mu sync.RWMutex
}

func (x *xmldb) cleanClassCache() {
	for {
		time.Sleep(time.Minute)
		x.mu.Lock()
		if time.Since(x.cacheHitLast) > 5*time.Minute {
			x.classCache = make(map[string][]byte)
		}
		x.mu.Unlock()
	}
}

func (x *xmldb) addToClassCache(ctx context.Context, class string, b []byte) {
	x.mu.Lock()
	defer x.mu.Unlock()
	configObjs := x.configClassess[utils.GetTraceFromContext(ctx).Uuid]
	if configObjs != nil {
		if _, ok := configObjs[class]; ok {
			return // Trace has something in cache
		}
	}

	dnConfigs := x.configs[utils.GetTraceFromContext(ctx).Uuid]
	if configObjs != nil {
		dns := x.classes[class]
		for dn := range dnConfigs {
			for _, cdn := range dns {
				if dn == cdn {
					return
				}
			}
		}
	}

	x.classCache[class] = b
}

func (x *xmldb) getFromClassCache(ctx context.Context, class string) (io.Reader, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()

	classCache, ok := x.classCache[class]
	if !ok {
		return nil, false
	}
	configObjs := x.configClassess[utils.GetTraceFromContext(ctx).Uuid]
	if configObjs != nil {
		if _, ok2 := configObjs[class]; ok2 {
			return nil, false // Trace has something in cache
		}
	}

	dnConfigs := x.configs[utils.GetTraceFromContext(ctx).Uuid]
	if dnConfigs != nil {
		dns := x.classes[class]
		for dn := range dnConfigs {
			for _, cdn := range dns {
				if dn == cdn {
					return nil, false
				}
			}
		}
	}

	x.cacheHitLast = time.Now()
	return bytes.NewBuffer(classCache), ok
}

func (x *xmldb) init() {
	x.classes = make(map[string][]string)
	x.configs = make(map[string]map[string]mxj.Map)
	x.configClassess = make(map[string]map[string][]string)
	x.classCache = make(map[string][]byte)

	go x.cleanClassCache()
}
func (x *xmldb) parseBolt() {
	x.init()
	opts := &bolt.Options{
		Timeout: 10 * time.Second,
	}

	var err error
	x.boltdb, err = bolt.Open(boltDbLoc, 0600, opts)
	if err != nil {
		fmt.Println("failed to open bolt db: ", err.Error())
		utils.SetErrorState(fmt.Sprintf("failed to open bolt db: %s", err.Error()))
		return
	} else {
		fmt.Println("bolt db opened")
	}

	// Bolt db expected to be mapping of dn -> xml
	err = x.boltdb.View(func(tx *bolt.Tx) error {
		// Assume bucket exists and has keys
		b := tx.Bucket([]byte(moxmlBucket))

		c := b.Cursor()

		for k, v := c.First(); k != nil; k, v = c.Next() {
			mv, merr := mxj.NewMapXml(v)
			if merr != nil {
				return merr
			}

			if len(mv) != 1 {
				return fmt.Errorf("bolt value contains more than one object")
			}

			// Map should be [classid] -> xml
			for class := range mv {
				x.classes[class] = append(x.classes[class], string(k))
			}
		}

		return nil
	})

	if err != nil {
		errstr := fmt.Sprintf("failed parsing of xml database: %s", err.Error())
		fmt.Println(errstr)
		utils.SetErrorState(errstr)
	}
}

// Retrieves all mos of given class and writes them to the input buffer
func (x *xmldb) getMosFromClass(ctx context.Context, name string, hierachical bool) mxj.Map {
	out := make(map[string]interface{})

	dns := x.classes[name]
	configObjs := x.configClassess[utils.GetTraceFromContext(ctx).Uuid]
	if configObjs != nil {
		if cfs, ok := configObjs[name]; ok {
			dns = append(dns, cfs...)
		}
	}

	for _, dn := range dns {
		mo, ok := x.getDn(ctx, dn, hierachical)
		if ok {
			if len(out) == 0 {
				out = mo
			} else {
				for k, v := range out {
					switch v := v.(type) {
					case []interface{}:
						for _, v2 := range mo {
							out[k] = append(v, v2)
						}
					case map[string]interface{}:
						for _, v2 := range mo {
							out[k] = []interface{}{v, v2}
						}
					}
				}
			}
		}
	}

	return out
}

// Returns list of dns matching given prefix
func (x *xmldb) getChildrenDns(ctx context.Context, dn string) []string {
	var out []string

	prefix := []byte(dn + "/")

	err := x.boltdb.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(moxmlBucket))

		if b == nil {
			return nil // Should never happen
		}
		c := b.Cursor()

		for k, _ := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, _ = c.Next() {
			out = append(out, string(k))
		}

		return nil
	})
	if err != nil {
		fmt.Println("db read in getDnPrefix failed: ", err.Error())
	}

	return out
}

// Retrieves the object with given dn from db, applies any configuration done and writes to input buffer.
// Returns whether object exists in db or not.
func (x *xmldb) getDn(ctx context.Context, dn string, resolveChildren bool) (mxj.Map, bool) {
	var mo mxj.Map

	err := x.boltdb.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(moxmlBucket))

		if b == nil {
			return nil // Should never happen
		}

		// Get db mo
		dbmo := b.Get([]byte(dn))

		// Check if mo has been configured
		confmo := x.getDnFromConfigs(ctx, dn)

		if dbmo != nil && confmo == nil {
			moTop, err := mxj.NewMapXml(dbmo)
			if err != nil {
				return err
			}
			if len(moTop) != 1 {
				return fmt.Errorf("db mo parse returned more than one mo")
			}

			mo = moTop
		} else if dbmo == nil && confmo != nil {
			mo = confmo
			adlog.MustFromContext(ctx).Sugar().Infof("config output: %+v", mo)
		} else if dbmo != nil && confmo != nil {
			logger := adlog.MustFromContext(ctx)
			// Merge the two
			dbmov, err := mxj.NewMapXml(dbmo)
			if err != nil {
				return err
			}

			if len(dbmov) != 1 {
				return fmt.Errorf("db mo parse returned more than one mo")
			}

			for k, v := range dbmov {
				mv := v.(map[string]interface{})
				for k, v := range confmo[k].(map[string]interface{}) {
					logger.Sugar().Debugf("applying key: %s with value: %v", k, v)
					mv[k] = v
				}
			}
			mo = dbmov
			logger.Sugar().Debugf("merged output: %+v", mo)
		}
		return nil
	})

	if err != nil {
		fmt.Print("error reading bolt db: ", err.Error())
	}

	if mo != nil && resolveChildren {
		logger := adlog.MustFromContext(ctx)
		logger.Info("resolving children")
		var dn string
		var parentNode map[string]interface{}
		for _, v := range mo {
			parentNode = v.(map[string]interface{})
			dn = parentNode["-dn"].(string)
		}
		// Find children
		childDns := x.getChildrenDns(ctx, dn)

		for _, childDn := range childDns {
			child, ok := x.getDn(ctx, childDn, resolveChildren)
			if ok {
				for k, v := range child {
					if sameclasschild, ok := parentNode[k]; ok {
						logger.Sugar().Infof("child already exists of this class: %s", k)
						switch sameclasschild := sameclasschild.(type) {
						case []interface{}:
							parentNode[k] = append(sameclasschild, v)
						case map[string]interface{}:
							parentNode[k] = []interface{}{sameclasschild, v}
						}
					} else {
						logger.Sugar().Infof("adding first child of class: %s", k)
						parentNode[k] = v
					}
				}
			}
		}
	}
	return mo, mo != nil
}

func (x *xmldb) getDnFromConfigs(ctx context.Context, dn string) mxj.Map {
	x.mu.RLock()
	defer x.mu.RUnlock()
	trace := utils.GetTraceFromContext(ctx)

	dmap, ok := x.configs[trace.Uuid]
	if !ok {
		return nil
	}

	mo, ok := dmap[dn]
	if !ok {
		return nil
	}
	return mo
}

func (x *xmldb) writeDn(ctx context.Context, dn string, inmap mxj.Map) {
	x.mu.Lock()
	defer x.mu.Unlock()
	trace := utils.GetTraceFromContext(ctx)

	dmap, ok := x.configs[trace.Uuid]
	if !ok {
		dmap = make(map[string]mxj.Map)
		x.configs[trace.Uuid] = dmap
	}

	mo, ok := dmap[dn]
	if !ok {
		dmap[dn] = inmap
	} else {
		for k, v := range inmap {
			mo[k] = v
		}
	}
}

var db *xmldb

// Server from which to fetch the xml payload
// TODO: Move off of dev machine to some shared/reliable repository
const dbServer = "http://10.193.37.120:8999/"

func init() {
	fmt.Println("in xml init")
	dbName := os.Getenv("DB_NAME")
	if dbName == "" {
		db = &xmldb{}
		db.init()
		return
	}

	// Get bolt db
	boltresp, err := http.Get(dbServer + dbName + ".bolt")
	if err != nil {
		utils.SetErrorState(fmt.Sprintf("failed to download bolt db: %s", err.Error()))
		return
	}

	if boltresp.StatusCode == http.StatusOK {
		err = os.Mkdir("/tmp", 0666)
		if err != nil {
			fmt.Println("failed to create tmp dir: ", err.Error()) // Ignore, might already exists
		}
		boltf, ferr := os.Create(boltDbLoc)
		if ferr != nil {
			utils.SetErrorState(fmt.Sprintf("failed to create bolt db: %s", err.Error()))
			return
		}

		_, ferr = io.Copy(boltf, boltresp.Body)
		if ferr != nil {
			utils.SetErrorState(fmt.Sprintf("failed to create bolt db: %s", err.Error()))
			return
		}

		ferr = boltresp.Body.Close()
		if ferr != nil {
			utils.SetErrorState(fmt.Sprintf("failed to create bolt db: %s", err.Error()))
			return
		}
		ferr = boltf.Close()
		if ferr != nil {
			utils.SetErrorState(fmt.Sprintf("failed to create bolt db: %s", err.Error()))
			return
		}
	} else {
		utils.SetErrorState(fmt.Sprintf("can't download bolt db: %d", boltresp.StatusCode))
		return
	}

	db = &xmldb{}
	db.parseBolt()
	// Build in memory mapping from bolt db for class -> xml map & dn -> xml
}

const boltDbLoc = "/tmp/bolt.db"

const moxmlBucket = "moxml"

func (p *XmlPlugin) Init(ctx context.Context) {
	// Create unique properties for this inventory
	trace := utils.GetTraceFromContext(ctx)

	switch trace.Platform {
	case "IMCM5", "IMCM4":
		// On IMC change rack unit serial and topSystem ip
		rack := make(map[string]interface{})
		rack["computeRackUnit"] = map[string]interface{}{"-serial": trace.Serial[0]}

		adlog.MustFromContext(ctx).Sugar().Infof("writing rack %+v", rack)

		db.writeDn(ctx, "sys/rack-unit-1", rack)

		top := make(map[string]interface{})
		top["topSystem"] = map[string]interface{}{"-address": trace.Ip[0]}
		db.writeDn(ctx, "sys", top)
	case "UCSFI":
		// On ucsm write serials to
		top := make(map[string]interface{})
		top["topSystem"] = map[string]interface{}{"-address": trace.Ip[0]}
		db.writeDn(ctx, "sys", top)

		switchId := 'A'
		for _, ser := range trace.Serial {
			ne := make(map[string]interface{})
			ne["networkElement"] = map[string]interface{}{"-serial": ser}
			db.writeDn(ctx, "sys/switch-"+string(switchId), ne)
			switchId = switchId + 1
		}
	}
}

func (p *XmlPlugin) Delegate(ctx context.Context, in adconnector.Body) (io.Reader, error) {
	logger := adlog.MustFromContext(ctx)
	dellock.Lock()
	defer dellock.Unlock()

	//logger.Info("in xml query plug")

	msg := &xmlMessage{}
	err := adio.JsonToObjectCtx(ctx, bytes.NewReader(in), msg)
	if err != nil {
		logger.Error("failed to unmarshal xml reqeust", zap.Error(err))
		return nil, err
	}

	if strings.Contains(msg.XmlRequest, "configResolveClass") {
		return p.handleConfigResolveClass(ctx, []byte(msg.XmlRequest))
	} else if strings.Contains(msg.XmlRequest, "configConfMo ") {
		return p.handleConfMo(ctx, []byte(msg.XmlRequest))
	} else if strings.Contains(msg.XmlRequest, "configResolveDn ") {
		return p.handleResolveDn(ctx, []byte(msg.XmlRequest))
	} else if strings.Contains(msg.XmlRequest, "configResolveChildren ") {
		return p.handleResolveChildren(ctx, []byte(msg.XmlRequest))
	}

	logger.Sugar().Infof("received unknown xml message: %s", msg.XmlRequest)

	return nil, fmt.Errorf("unkown message type")
}

func getIsHierarchical(ctx context.Context, m mxj.Map) bool {
	var hierch bool
	for _, v := range m {
		mv, ok := v.(map[string]interface{})
		if ok {
			hierchStr, ok := mv["-inHierarchical"].(string)
			if ok {
				if hierchStr == "True" || hierchStr == "true" {
					hierch = true
				}
			}
		}
	}
	return hierch
}

func (p *XmlPlugin) handleConfigResolveClass(ctx context.Context, in []byte) (io.Reader, error) {
	logger := adlog.MustFromContext(ctx)
	mv, err := mxj.NewMapXml(in) // unmarshal
	if err != nil {
		logger.Error("failed to parse config xml", zap.Error(err))
	}

	logger.Sugar().Debugf("input map: %+v", mv)

	var classId string
	for _, v := range mv {
		momv, ok := v.(map[string]interface{})
		if ok {
			classId, ok = momv["-classId"].(string)
			if !ok {
				fmt.Println("no dn found on config resolve")
			}
		}
	}

	hierch := getIsHierarchical(ctx, mv)

	if classId == "" {
		logger.Warn("did not receive class id")
		return nil, fmt.Errorf("no class id to resolve")
	}

	if hierch {
		logger.Info("resolving all children")
	} else {
		cachebuf, ok := db.getFromClassCache(ctx, classId)
		if ok {
			return cachebuf, nil
		}
	}

	outConfigs := make(map[string]interface{})
	mos := db.getMosFromClass(ctx, classId, hierch)

	outConfigs["outConfigs"] = mos

	out := bytes.NewBuffer(nil)
	p.encodeToXml(ctx, outConfigs, "configResolveClass", out)
	cbuf := make([]byte, out.Len())
	copy(cbuf, out.Bytes())
	db.addToClassCache(ctx, classId, cbuf)

	//buf := bytes.NewBuffer(x.Bytes())
	//logger.Sugar().Infof("config resolve out: %s", buf.String())

	return out, err
}

func (p *XmlPlugin) encodeToXml(ctx context.Context, in map[string]interface{}, key string, out *bytes.Buffer) {

	out.WriteString(`<` + key)

	n := 0
	for k, v := range in {
		if strings.HasPrefix(k, "-") {
			switch v := v.(type) {
			case string:
				out.WriteString(fmt.Sprintf(` %s="%s"`, k[1:], v))
			case []interface{}:
			case map[string]interface{}:
			case mxj.Map:
			default:
				adlog.MustFromContext(ctx).Sugar().Panicf("unknown type : %s", reflect.TypeOf(v).String())
			}
			n++
		}
	}

	if n == len(in) {
		out.WriteString(`/>`)
		return
	}

	out.WriteString(">")
	for k, v := range in {
		if strings.HasPrefix(k, "-") {
			continue
		}

		switch v := v.(type) {
		case []interface{}:
			for _, vv := range v {
				p.encodeToXml(ctx, vv.(map[string]interface{}), k, out)
			}
		case map[string]interface{}:
			p.encodeToXml(ctx, v, k, out)
		case mxj.Map:
			p.encodeToXml(ctx, v, k, out)
		default:
			adlog.MustFromContext(ctx).Sugar().Panicf("unknown type : %s", reflect.TypeOf(v).String())
		}
	}

	out.WriteString(`</` + key + `>`)
}

func (p *XmlPlugin) handleConfMo(ctx context.Context, in []byte) (io.Reader, error) {
	logger := adlog.MustFromContext(ctx)

	// Construct a valid response.
	mv, err := mxj.NewMapXml(in) // unmarshal
	if err != nil {
		logger.Error("failed to parse config xml", zap.Error(err))
	}

	logger.Sugar().Infof("input map: %+v", mv)

	// Validate dn is present
	valid := false
	if confMo, ok := mv["configConfMo"].(map[string]interface{}); ok {
		if dn, ok := confMo["-dn"].(string); ok && dn != "" {
			valid = true
		}
	}

	if !valid {
		return nil, fmt.Errorf("unable to validate conf mo input")
	}

	var inputMo map[string]interface{}
	// Look for the mo in inConfig
	inConfs, err := mv.ValuesForPath("configConfMo.inConfig")
	if err != nil {
		logger.Error("failed to get path value", zap.Error(err))
		return nil, err
	}

	for _, conf := range inConfs {
		if confmap, ok := conf.(map[string]interface{}); ok {
			inputMo = confmap
			logger.Sugar().Infof("config map: %+v", confmap)

			break
		}
	}

	applyConfig(ctx, inputMo)

	dn := mv["configConfMo"].(map[string]interface{})["-dn"].(string)
	db.writeDn(ctx, dn, inputMo)

	outConfigs := make(map[string]interface{})

	mo, ok := db.getDn(ctx, dn, false) // TODO: Get resolve from input
	if ok {
		outConfigs["outConfig"] = mo
	} else {
		outConfigs["outConfig"] = nil
	}

	buf := new(bytes.Buffer)
	err = mxj.Map(outConfigs).XmlWriter(buf, "configConfMo")
	if err != nil {
		logger.Error("failed to marshal xml", zap.Error(err))
		return nil, err
	}

	//logger.Sugar().Infof("returning out: %s", string(outConfXml))
	return buf, err
}

// Apply configuration to the incoming mo, applying any
func applyConfig(ctx context.Context, inmap map[string]interface{}) {
	logger := adlog.MustFromContext(ctx)

	if len(inmap) != 1 {
		logger.Info("more than one mo in input")
		return
	}
	var moclass string
	var mo map[string]interface{}
	for k, v := range inmap {
		moclass = k
		var ok bool
		mo, ok = v.(map[string]interface{})
		if !ok {
			logger.Info("input isn't an mo")
			return
		}
	}
	logger.Sugar().Infof("applying config to %s. in: %+v", moclass, mo)

	switch moclass {
	case "equipmentLocatorLed":
		adminState, ok := mo["-adminState"].(string)
		if ok {
			// Match oper state to admin state
			mo["-operState"] = adminState
			adlog.MustFromContext(ctx).Sugar().Infof("changed operstate to: %s", adminState)
		} else {
			logger.Sugar().Infof("no adminState in input: %v", mo["-adminState"])
		}
	case "computeRackUnit":
		adminPower, ok := mo["-adminPower"]
		if ok {
			switch adminPower {
			case "soft-shut-down", "down":
				mo["-operPower"] = "off"
			case "up":
				mo["-operPower"] = "on"
			}
		} else {
			logger.Info("no power state in rack input")
		}
	}
}

func (p *XmlPlugin) handleResolveDn(ctx context.Context, in []byte) (io.Reader, error) {
	logger := adlog.MustFromContext(ctx)

	// Construct a valid response.
	mv, err := mxj.NewMapXml(in) // unmarshal
	if err != nil {
		logger.Error("failed to parse config xml", zap.Error(err))
	}

	logger.Sugar().Infof("input map: %+v", mv)

	var dn string
	for _, v := range mv {
		momv, ok := v.(map[string]interface{})
		if ok {
			dn, ok = momv["-dn"].(string)
			if !ok {
				fmt.Println("no dn found on config resolve")
			}
		}
	}

	hierch := getIsHierarchical(ctx, mv)

	outConfigs := make(map[string]interface{})

	mo, exists := db.getDn(ctx, dn, hierch)
	if exists {
		outConfigs["outConfig"] = mo
	} else {
		outConfigs["outConfig"] = nil
	}

	outConfigs["-dn"] = dn

	buf := new(bytes.Buffer)
	err = mxj.Map(outConfigs).XmlWriter(buf, "configResolveDn")
	if err != nil {
		logger.Error("failed to marshal xml", zap.Error(err))
		return nil, err
	}

	//logger.Sugar().Infof("returning resolve dn out: %s", string(outConfXml))
	return buf, nil
}

func (p *XmlPlugin) handleResolveChildren(ctx context.Context, in []byte) (io.Reader, error) {
	logger := adlog.MustFromContext(ctx)

	// Construct a valid response.
	mv, err := mxj.NewMapXml(in) // unmarshal
	if err != nil {
		logger.Error("failed to parse config xml", zap.Error(err))
	}

	logger.Sugar().Infof("resolve children input map: %+v", mv)
	var dn string
	for _, v := range mv {
		momv, ok := v.(map[string]interface{})
		if ok {
			dn, ok = momv["-inDn"].(string)
			if !ok {
				fmt.Println("no dn found on config resolve")
			}
		}
	}

	hierch := getIsHierarchical(ctx, mv)

	logger.Sugar().Infof("resolving children of dn: %s", dn)

	dns := db.getChildrenDns(ctx, dn)

	outConfigs := make(map[string]interface{})

	outConfigs["-inDn"] = dn
	outConfigs["-response"] = "yes"

	mos := make(map[string]interface{})
outer:
	for _, children := range dns {
		mo, ok := db.getDn(ctx, children, hierch)
		if !ok {
			fmt.Println("could not find: ", children)
		} else {
			for class, emos := range mos {
				for k, v := range mo {
					if k == class {
						switch emos := emos.(type) {
						case []interface{}:
							mos[class] = append(emos, v)
						case map[string]interface{}:
							mos[class] = []interface{}{emos, v}
						}
						continue outer
					}
				}
			}

			for k, v := range mo {
				mos[k] = v
			}
		}
	}

	outConfigs["outConfigs"] = mos
	buf := new(bytes.Buffer)

	err = mxj.Map(outConfigs).XmlWriter(buf, "configResolveChildren")
	if err != nil {
		logger.Error("failed to marshal xml", zap.Error(err))
		return nil, err
	}

	//logger.Sugar().Infof("returning resolve children out: %s", string(outConfXml))
	return buf, nil
}

// Imports an inventory configuration from the northbound of the emulator server
func (p *XmlPlugin) ImportConfiguration(ctx context.Context, config *utils.InventoryConfiguration) error {
	logger := adlog.MustFromContext(ctx)
	logger.Sugar().Infof("importing config: %+v", config)
	changedClasses := make(map[string]struct{})
	for _, obj := range config.Objects {
		if obj.Identity == "" {
			return fmt.Errorf("%+v has no identity", obj)
		}
		if obj.ObjectType == "" {
			return fmt.Errorf("%+v has no object type", obj)
		}
		switch obj.Action {
		case utils.UpdateInventory, utils.AddInventory:
			emo, exists := db.getDn(ctx, obj.Identity, false)
			if obj.Action == utils.AddInventory && exists {
				return fmt.Errorf("object to add: %s exists", obj.Identity)
			} else if obj.Action == utils.UpdateInventory {
				if !exists {
					return fmt.Errorf("object to update: %s does not exist", obj.Identity)
				}

				for k := range emo {
					if k != obj.ObjectType {
						return fmt.Errorf("existing object type: %s does not match incoming %s", k, obj.ObjectType)
					}
				}
			}

			// Convert json object to xm
			xmlObj := make(map[string]interface{})
			for k, v := range obj.Properties {
				xmlObj["-"+strings.ToLower(k[:1])+k[1:]] = v
			}
			db.writeDn(ctx, obj.Identity, map[string]interface{}{obj.ObjectType: xmlObj})

			if mo, ok := db.getDn(ctx, obj.Identity, false); ok {
				logger.Sugar().Infof("Wrote object: %+v", mo)
				for k := range mo {
					changedClasses[k] = struct{}{}
					break
				}
			}

			if obj.Action == utils.AddInventory {
				dcfs, ok := db.configClassess[utils.GetTraceFromContext(ctx).Uuid]
				if !ok {
					db.configClassess[utils.GetTraceFromContext(ctx).Uuid] = make(map[string][]string)
					dcfs = db.configClassess[utils.GetTraceFromContext(ctx).Uuid]
				}
				dcfs[obj.ObjectType] = append(dcfs[obj.ObjectType], obj.Identity)
			}
		/*case utils.AddInventory:
		return fmt.Errorf("add not supported right now, check back later")*/
		case utils.DeleteInventory:
			return fmt.Errorf("delete not supported right now, check back later")
		default:
			return fmt.Errorf("unknown action: %s", obj.Action)
		}
	}

	// Trigger the dejavu job to run right away. TODO: Emit an event.
	for class := range changedClasses {
		jobName := "dejavu:" + strings.Title(class)
		if class == "faultInst" {
			// Special handling for faults, the job name is different for some reason
			jobName = "dejavu:FaultInstance"
		}
		if !p.jobPlugin.RunJobNow(ctx, jobName) {
			adlog.MustFromContext(ctx).Sugar().Infof("couldn't find job: %s", jobName)
		}
	}
	return nil
}

// Returns a list of class objects
func (p *XmlPlugin) ResolveClass(ctx context.Context, className string) mxj.Map {
	dellock.Lock()
	defer dellock.Unlock()
	mos := db.getMosFromClass(ctx, className, false)
	return mos
}
