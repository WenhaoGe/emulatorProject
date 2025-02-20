package plugins

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"

	"io"
	"math"
	"math/rand"
	"time"

	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adconnector"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adcore"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adio"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adlog"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adutil"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

// Defines an object to collect from an Inventory API, includes all meta required
// to construct an Intersight conformant object.
type InventoryEtlObject struct {
	// The Intersight object type of the output object.
	// e.g. 'compute.Blade'
	ObjectType string

	// List of fields in the output that in combination uniquely identify
	// this object in the platform.
	// e.g. {"Dn"} or {"Uuid"}
	IdentityFields []string

	// Set of fields to use when merging multiple apis into this object.
	// Can be used when all apis do not contain all Intersight Identity fields.
	// Optional: If none defined will fallback to the objects IdentityFields
	MergeIdentities []string

	// Inclusion filters.
	// If defined keys in this map must exist in the output mo and match
	// at least one of the given regex expressions.
	InclusionFilters map[string][]string

	// List of properties to extract from the platform.
	Properties []*InventoryEtlProperty

	// List of relationships to construct from the platform output.
	Relationships []*InventoryEtlRelationship
}

// Describes a relationship of an object.
type InventoryEtlRelationship struct {
	// The name of the relationship field.
	Name string

	// The type of the relation.
	ObjectType string

	// This relationship is a collection.
	Collection bool

	// Marks that this relationship should only be set if this other relationship is not set
	ExclusiveOr string

	// List of selectors used to construct an odata filter
	// that identifies a single peer object.
	// Resultant selector for example: 'Dn eq "sys/chassis-1/blade-7"'
	Selectors []*InventoryRelSelector
}

// A relationship selector
type InventoryRelSelector struct {
	// The property name to match in the relationship mo.
	Prop string

	// Property must exist as a key in the output object.
	// If not defined SourceProp is assumed to be 'Prop'
	SourceProp string

	// The operation to apply. e.g. "eq"
	Op string

	// The regex to transform Prop value. e.g. a regex to trim a ucsm dn to construct the parent dn.
	// Deprecated: Replaced with SelectorPropBuilder
	Regex string

	// Describes how the property value in the selector should be constructed
	Builder *SelectorPropBuilder
}

// TODO: Validate the input for this
type SelectorPropBuilder struct {
	Type string

	// The source prop to apply operations to, if empty uses the Prop on the selector.
	SourceProp string

	// Match string type
	// The regex match string to apply to SourceProp
	MatchRegex string

	// Builder type
	BuildString string

	// Trim type

	// The value in SourceProp after which is trimmed (inclusively)
	TrimAfter string
}

// Describes the ETL for a property.
type InventoryEtlProperty struct {
	// The name of the property in the output.
	Name string

	// The type of the property.
	// TODO: Currently only supports scalar types (e.g. int, bool)
	Type string

	// The API that is the source of this property.
	Api string

	// Describes how to extract this property from the API
	Extract InventoryExtract

	// Marks that the property is not to be committed and is only used to build other properties or relationships.
	BuildOnly bool
}

// Describes a platform API that supplies the inventory
type InventoryApi struct {
	Name string

	// Type of the API. e.g. http, cli or custom platform.
	Type string

	// The address of the API on the platform.
	Address string

	// The resource on the API to query
	Resource string

	// Jmespath projection to apply before extracting objects
	Projection string
}

// Describes how a property can be extracted from an API.
type InventoryExtract struct {
	// Type of extraction.
	Type string

	// Path to extract from.
	Path string

	// Build regex
	BuildString string

	// TODO: regex/transform
	Regex string
}

// Init message for an inventory collection.
type InventoryEtlInit struct {
	// Interval at which to collect all objects in the 'Objects' list from the platform apis.
	Frequency time.Duration

	// List of objects to collect.
	Objects []*InventoryEtlObject

	// List of APIs that objects are inventoried from.
	Apis []*InventoryApi
}

type CommonInventory struct {
	// A cache which maintains a copy of reported inventory. Each inventory item which is
	// reported to the CommonInventory plugin is compared against the cache and when there
	// is a difference the delta is applied to the cache and notified to Intersight
	// through the inventory stream.
	diffCache *cache

	// The devices 'inventory' created.
	// TODO: Derive from some sort of meta.
	inventory map[string][]map[string]interface{}

	// Mapping between object type and list of identity fields.
	// Populated on receipt of an inventory init message
	objectIdentities map[string][]string
	objectIdsMu      sync.Mutex

	// Cancel function for stopping any currently running inventories
	cancel context.CancelFunc
	wg     sync.WaitGroup
	mu     sync.Mutex
}

// Inventory collection plugins create instances of InventoryReport to report
// inventory to the CommonInventory plugin. The inventory is reported through
// the channel which is provided to the collection plugin when CollectInventory()
// is invoked to start collection.
type InventoryReport struct {
	// A list of objects which conform to the Intersight object model.
	// The objects will be inserted/updated in Intersight.
	// The 'ObjectType' field is mandatory. Fields which are marked as Identity
	// fields in the Intersight model are mandatory, remaining fields are optional
	// and may be omitted. Eg if they are known to have not changed since being
	// previously reported.
	Upserts []map[string]interface{}

	// A list of objects which conform to the Intersight object model.
	// The objects will be deleted from Intersight.
	// The 'ObjectType' field is mandatory. Fields which are marked as Identity
	// fields in the Intersight model are mandatory, this is how Intersight will
	// identify the object to be deleted. Other fields are ignored.
	Deletes []map[string]interface{}

	// Indicates whether the items contain a full or partial inventory.
	// Upserts and Deletes are always applied to the CommonInventory's
	// cache. Additionally in the case of Full=True, any objects in the
	// cache but not in the Upserts list are removed from the cache.
	Type string
}

type collectionMessage struct {
	DeleteMos interface{}
	Type      string
	UpsertMos interface{}
}

const (
	ConnectorCommonInventoryCollectionTypeFullStart   = "fullStart"
	ConnectorCommonInventoryCollectionTypeFullSegment = "fullSegment"
	ConnectorCommonInventoryCollectionTypeFullEnd     = "fullEnd"
	ConnectorCommonInventoryCollectionTypeIncremental = "incremental"
)

// A cache of inventoried objects. The key of the outer map is calculated
// from object identity and the inner map[string]interface{} is a
// representation of an Intersight-compliant MO.
//
// Access to cache must be performed via accessors which are concurrency-safe.
type cache struct {
	objects map[string]map[string]interface{}
	lock    *sync.Mutex
}

// Returns the value in the cache for 'key'. The returned bool
// indicates if 'key' is present in the cache.
func (c *cache) get(key string) (map[string]interface{}, bool) {
	c.lock.Lock()
	defer c.lock.Unlock()
	ret, ok := c.objects[key]
	return ret, ok
}

// Set entry 'key' in the cache to 'value'.
func (c *cache) put(key string, value map[string]interface{}) {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.objects[key] = value
}

// Delete entry 'key' from cache.
func (c *cache) delete(key string) {
	c.lock.Lock()
	defer c.lock.Unlock()
	delete(c.objects, key)
}

// Return a list of all keys present in the cache.
func (c *cache) keys() []string {
	c.lock.Lock()
	defer c.lock.Unlock()
	ret := make([]string, len(c.objects))
	i := 0
	for k := range c.objects {
		ret[i] = k
		i++
	}
	return ret
}

// Drops the entire cache contents
func (c *cache) drop() {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.objects = make(map[string]map[string]interface{})
}

// Implement plugins.Plugin interface
func (p *CommonInventory) Init(ctx context.Context) {
	p.diffCache = &cache{objects: make(map[string]map[string]interface{}), lock: &sync.Mutex{}}

	p.objectIdentities = make(map[string][]string)
}

// Implement plugins.Plugin interface
//
// StreamDelegate should be used instead of Delegate
func (p *CommonInventory) Delegate(ctx context.Context, in adconnector.Body) (io.Reader, error) {
	return nil, nil
}

const objsPerType = 100

// Simple inventory create
func (p *CommonInventory) createInventory(ctx context.Context, msg *InventoryEtlInit) {
	//logger := adlog.MustFromContext(ctx)
	p.inventory = make(map[string][]map[string]interface{})
	for _, obj := range msg.Objects {
		p.inventory[obj.ObjectType] = make([]map[string]interface{}, objsPerType)
		for i := 0; i < objsPerType; i++ {
			newobj := make(map[string]interface{})
			newobj[adcore.BaseMoObjectTypePropName] = obj.ObjectType
			for _, prop := range obj.Properties {
				if prop.BuildOnly {
					continue // ?
				}
				switch prop.Type {
				case "double":
					fallthrough
				case "float":
					newobj[prop.Name] = float64(rand.Intn(math.MaxInt32)) / (float64(rand.Intn(math.MaxInt32)) + 1)
				case "integer":
					newobj[prop.Name] = rand.Intn(math.MaxInt32)
				case "string":
					newobj[prop.Name] = adutil.NewRandStr(16)
				}
			}
			p.inventory[obj.ObjectType][i] = newobj
		}
	}
	//logger.Sugar().Infof("inventory before rels: %+v", p.inventory)

	for _, obj := range msg.Objects {
		for i := 0; i < objsPerType; i++ {
			for _, rel := range obj.Relationships {
				//logger.Sugar().Infof("adding rel of type: %s", rel.ObjectType)

				indx := rand.Intn(objsPerType)
				//logger.Sugar().Infof("getting idx: %d", indx)
				peerObs, ok := p.inventory[rel.ObjectType]
				if !ok {
					continue
				}
				peer := peerObs[indx]
				var sel string
				for _, selector := range rel.Selectors {
					sel = fmt.Sprintf("%s %s '%s'", selector.Prop, selector.Op, peer[selector.Prop])
				}

				//logger.Sugar().Infof("constructed selector %s", sel)

				p.inventory[obj.ObjectType][i][rel.Name] = &adcore.MoRef{
					ObjectType: rel.ObjectType,
					Selector:   sel,
				}
			}
		}
	}

}

// Change the inventory a little
func (p *CommonInventory) modInventory(ctx context.Context, msg *InventoryEtlInit) {
	logger := adlog.MustFromContext(ctx)
	totalChanges := 0
	for _, objs := range msg.Objects {
		for i, obj := range p.inventory[objs.ObjectType] {
			if rand.Intn(100) < 50 {
				continue
			}
			newObj := make(map[string]interface{})
			for k, v := range obj {
				newObj[k] = v
			}
		modloop:
			for k, v := range newObj {
				if k == adcore.BaseMoObjectTypePropName {
					continue
				}
				for _, idfield := range objs.IdentityFields {
					if k == idfield {
						continue modloop
					}
				}
				switch v.(type) {
				case float64:
					newObj[k] = newObj[k].(float64) + float64(rand.Intn(math.MaxInt16))
				case int:
					newObj[k] = newObj[k].(int) + rand.Intn(math.MaxInt16)
				case string:
					newObj[k] = adutil.NewRandStr(16)
				}
				totalChanges++
			}

			p.inventory[objs.ObjectType][i] = newObj
			//logger.Sugar().Infof("new\n %+v \nvs \nold %+v", newObj, obj)

		}
	}
	logger.Sugar().Infof("made %d changes", totalChanges)

}

func (p *CommonInventory) collectInventory(ctx context.Context, msg *InventoryEtlInit, outCh chan *InventoryReport) {
	logger := adlog.MustFromContext(ctx)
	first := true
	for {
		if !first {
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Minute):
			}

			p.modInventory(ctx, msg)
		}

		first = false

		logger.Info("sending some inventory")

		firstout := &InventoryReport{}

		// Send start
		firstout.Type = ConnectorCommonInventoryCollectionTypeFullStart
		outCh <- firstout

		j := 0
		for objname, objs := range p.inventory {
			i := 0
			for {
				out := &InventoryReport{}
				out.Type = ConnectorCommonInventoryCollectionTypeFullSegment

				if i+50 < len(p.inventory) {
					out.Upserts = objs[i : i+50]
				} else {
					out.Upserts = objs[i:]

				}

				i = i + len(out.Upserts)

				if i >= len(objs)-1 {
					logger.Sugar().Infof("done sending %s", objname)
					if j == len(p.inventory)-1 {
						logger.Info("sending end")
						out.Type = ConnectorCommonInventoryCollectionTypeFullEnd
					}

					outCh <- out
					break
				}
				outCh <- out
			}

			j++
		}
	}
}

type startMessage struct {
	Input      []byte `bson:"Input"`
	PluginName string `bson:"PluginName"`
}

// Start collection of inventory. The 'initMsg' describes the mechanics by which
// inventory should be collected and transformed from a platform specific to
// Intersight-compliant representation. CommonInventory maintains a stateful cache of
// reported inventory and reports delta changes to Intersight through 'outCh'. Non-fatal
// errors are reported through 'errCh' and logged (and possibly reacted to) in Intersight.
// Intersight may send subsequent instructions through 'inCh' to modify the behavior of
// the inventory collection without breaking and re-establishing the collection stream.
func (p *CommonInventory) StreamDelegate(ctx context.Context, inCh, outCh, errCh chan adconnector.Body,
	streamName string, in adconnector.Body) error {
	logger := adlog.MustFromContext(ctx)

	initMsg := &startMessage{}
	err := adio.JsonToObjectCtx(ctx, bytes.NewReader(in), initMsg)
	if err != nil {
		logger.Error("Unable to deserialize common inventory message json", zap.Error(err))
		return err
	}

	etlmsg := &InventoryEtlInit{}
	err = adio.JsonToObjectCtx(ctx, bytes.NewReader(initMsg.Input), etlmsg)
	if err != nil {
		logger.Error("Unable to deserialize etl message json", zap.Error(err))
		return err
	}
	if len(etlmsg.Objects) == 0 {
		return errors.New("MO identity information not present in init message")
	}
	p.objectIdsMu.Lock()
	for _, obj := range etlmsg.Objects {
		// Sort the identify fields to ensure stability in ordering. Unstable ordering
		// will cause duplicate reporting of inventory.
		sort.Strings(obj.IdentityFields)
		p.objectIdentities[obj.ObjectType] = obj.IdentityFields
	}
	p.objectIdsMu.Unlock()

	logger.Sugar().Infof("got etl message : %+v", etlmsg)
	p.createInventory(ctx, etlmsg)

	// TODO: Enforcing one inventory collection stream per device connector.
	// TODO: On claim device needs to re-send inventory (clearing cache and closing previous stream)
	// TODO: This is a temporary workaround to get to a clean state and re-collect when cloud re-opens stream.
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		p.cancel()
		p.wg.Wait()
		p.diffCache.drop()
	}

	var doctx context.Context
	doctx, p.cancel = context.WithCancel(ctx)

	invCh := make(chan *InventoryReport)

	// Start "collector"
	go p.collectInventory(ctx, etlmsg, invCh)
	p.wg.Add(1)
	go p.processInventory(doctx, invCh, outCh)
	return nil
}

// Reprocess missing relationships, adding the objects to the cache if they now resolve all relations
func (p *CommonInventory) reProcessMissingRelations(ctx context.Context,
	relmissing []map[string]interface{}, upserts *[]map[string]interface{}) {
	logger := adlog.MustFromContext(ctx)
	key := &bytes.Buffer{}
	relkey := &bytes.Buffer{}

	// Count number of different object types in missing, to determine number of loops to re-resolve
	uObjects := make(map[string]struct{})
	for _, inv := range relmissing {
		motype, ok := inv[adcore.BaseMoObjectTypePropName].(string)
		if ok {
			uObjects[motype] = struct{}{}
		}
	}

	for i := 0; i < len(uObjects); i++ {
		if len(relmissing) == 0 {
			break
		}

		stillmissing := 0 // Reslice the missing mos retaining only those still missing
		for _, inv := range relmissing {
			err := p.constructKey(ctx, inv, key)
			if err != nil {
				logger.Sugar().Errorf("cache key construction failed : %v", err)
				continue
			}

			logger.Sugar().Debugf("re processing mo : %s", key)
		proploop:
			for _, v := range inv {
				switch v := v.(type) {
				case []*adcore.MoRef:
					allFound := true
					for _, moref := range v {
						if !p.resolveRelationship(ctx, moref, relkey) {
							allFound = false
						}
					}

					if allFound {
						logger.Sugar().Debugf("found rel after %d loop: %s", i, relkey.String())
						p.diffCache.put(key.String(), inv)
						*upserts = append(*upserts, inv) // produce to stream
						// TODO: Only looking at a single relationship right now
						break proploop
					} else {
						relmissing[stillmissing] = inv
						stillmissing++
					}
				case *adcore.MoRef:
					if !p.resolveRelationship(ctx, v, relkey) {
						relmissing[stillmissing] = inv
						stillmissing++
					} else {
						logger.Sugar().Debugf("found rel after %d loop: %s", i, relkey.String())
						p.diffCache.put(key.String(), inv)
						*upserts = append(*upserts, inv) // produce to stream
						// TODO: Only looking at a single relationship right now
						break proploop
					}
				}
			}
		}

		relmissing = relmissing[:stillmissing]
	}

	for _, inv := range relmissing {
		logger.Sugar().Infof("unable to find rel still at end: %+v", inv)
	}

}

// Resolve the moref in the input to an object in the cache.
// If object does not exist or moref is invalid false is returned
func (p *CommonInventory) resolveRelationship(ctx context.Context, moref *adcore.MoRef, relkey *bytes.Buffer) bool {
	logger := adlog.MustFromContext(ctx)
	relkey.Reset()

	if moref.Selector == "" {
		return false
	}

	relkey.WriteString("ObjectType=" + moref.ObjectType)
	// TODO: Handle multiple selectors
	idparts := strings.Split(moref.Selector, " eq ")
	if len(idparts) == 2 {
		relkey.WriteString(":" + idparts[0] + "=" + strings.Trim(idparts[1], `'`))
	} else {
		logger.Sugar().Infof("unable to parse rel selector: %s", moref.Selector)
		return false // TODO: Should the object be dropped here?
	}

	if _, relexists := p.diffCache.get(relkey.String()); !relexists {
		logger.Sugar().Debugf("unable to find rel: %s", relkey.String())
		// Don't insert into cache yet, wait until rel is found
		return false
	}

	return true
}

// Process inventory received from the collection plugin via 'invCh', evaluate against
// the diffCache and send deltas to Intersight via inventory stream via 'outCh'.
func (p *CommonInventory) processInventory(ctx context.Context, invCh chan *InventoryReport, outCh chan adconnector.Body) {
	logger := adlog.MustFromContext(ctx)
	defer p.wg.Done()
	defer close(outCh)

	fullInventoryKeys := make(map[string]bool)
	var relmissing []map[string]interface{}

	// Send a full inventory as the first set of messages, after that send incremental updates.
	sendFull := true

	for {
		select {
		case <-ctx.Done():
			logger.Info("inventory collection stopped")
			return
		case invItem, ok := <-invCh:
			if !ok {
				logger.Info("collector closed output channel")
				return
			}
			var upserts []map[string]interface{}
			var deletes []map[string]interface{}

			relkey := &bytes.Buffer{}
			key := &bytes.Buffer{}

		invloop:
			for _, inv := range invItem.Upserts {
				relkey.Reset()

				err := p.constructKey(ctx, inv, key)
				if err != nil {
					logger.Sugar().Errorf("cache key construction failed : %v", err)
					continue
				}

				// Check that the incoming mos relationships exist in the cache already, if not hold off sending.
				for _, v := range inv {
					switch v := v.(type) {
					case []*adcore.MoRef:
						for _, moref := range v {
							// Figure out how to cache lookup from selector.
							if !p.resolveRelationship(ctx, moref, relkey) {
								relmissing = append(relmissing, inv)
								continue invloop
							}
						}

					case *adcore.MoRef:
						// Figure out how to cache lookup from selector.
						if !p.resolveRelationship(ctx, v, relkey) {
							relmissing = append(relmissing, inv)
							continue invloop
						}
					}

				}

				cacheInv, ok := p.diffCache.get(key.String())
				if ok {
					diff := p.calculateDiff(inv, cacheInv)
					if diff != nil {
						p.diffCache.put(key.String(), inv)
						upserts = append(upserts, diff)
					}
				} else {
					p.diffCache.put(key.String(), inv)
					upserts = append(upserts, inv) // produce to stream
				}
			}

			for _, inv := range invItem.Deletes {
				err := p.constructKey(ctx, inv, key)
				if err != nil {
					logger.Sugar().Errorf("cache key construction failed : %v", err)
					continue
				}
				_, ok := p.diffCache.get(key.String())
				if ok {
					p.diffCache.delete(key.String())
					deletes = append(deletes, inv) // produce to stream
				}
			}

			// At the end of the collection re-evaluate all missing relationships.
			if invItem.Type == ConnectorCommonInventoryCollectionTypeFullEnd ||
				invItem.Type == ConnectorCommonInventoryCollectionTypeIncremental {
				p.reProcessMissingRelations(ctx, relmissing, &upserts)
				relmissing = relmissing[:0]
			}

			// In the case of full inventory, remove any item which is in the cache
			// but not in the newly reported inventory.

			if invItem.Type == ConnectorCommonInventoryCollectionTypeFullStart ||
				invItem.Type == ConnectorCommonInventoryCollectionTypeFullSegment ||
				invItem.Type == ConnectorCommonInventoryCollectionTypeFullEnd {

				for _, v := range invItem.Upserts {
					err := p.constructKey(ctx, v, key)
					if err != nil {
						logger.Sugar().Errorf("cache key construction failed : %v", err)
						continue
					}
					//logger.Sugar().Infof("adding key to full inv: %s", key.String())
					fullInventoryKeys[key.String()] = true
				}
			}

			if invItem.Type == ConnectorCommonInventoryCollectionTypeFullEnd {
				for _, key := range p.diffCache.keys() {
					if _, ok := fullInventoryKeys[key]; !ok {
						inv, _ := p.diffCache.get(key)
						deletes = append(deletes, inv) // produce to stream

						p.diffCache.delete(key)
						logger.Sugar().Infof("removed item from cache as a result of full inventory : %s", key)
					}
				}
				fullInventoryKeys = make(map[string]bool)
			}

			if len(upserts) > 0 || len(deletes) > 0 || (sendFull &&
				(invItem.Type == ConnectorCommonInventoryCollectionTypeFullEnd ||
					invItem.Type == ConnectorCommonInventoryCollectionTypeFullStart)) {
				//logger.Sugar().Infof("send inventory changes to intersight: upserts=%d deletes=%d",
				//	len(upserts), len(deletes))

				for {
					msg := &collectionMessage{}
					if !sendFull {
						msg.Type = ConnectorCommonInventoryCollectionTypeIncremental
					} else {
						msg.Type = invItem.Type
					}

					if len(upserts) > 100 {
						msg.UpsertMos = upserts[:100] // TODO: Program batch size from cloud.
						upserts = upserts[100:]
						if sendFull {
							msg.Type = ConnectorCommonInventoryCollectionTypeFullSegment
						} else {
							msg.Type = ConnectorCommonInventoryCollectionTypeIncremental // Overwrite as this will be incremental
						}
					} else {
						msg.UpsertMos = upserts
						upserts = nil
					}

					msg.DeleteMos = deletes

					//logger.Sugar().Infof("sending inventory to cloud: %+v", msg)

					logger.Sugar().Infof("sending type %s with %d upserts, %d deletes",
						msg.Type, len(msg.UpsertMos.([]map[string]interface{})), len(msg.DeleteMos.([]map[string]interface{})))
					outCh <- adio.ObjectToJson(ctx, msg, nil)

					if len(upserts) == 0 {
						break
					}
				}

				if invItem.Type == ConnectorCommonInventoryCollectionTypeFullEnd {
					// Only send full once at stream start, rely on incremental updates after.
					// TODO: Allow cloud to request a full inventory collection.
					sendFull = false
				}
			}
		}
	}
}

// Calculates a diff between two objects, returning the properties in 'new' that are different from ones in 'old'
// If a difference is detected, returned object will include the identity properties of the object
// TODO: Allocates a new object for each diff, can that be avoided?
func (p *CommonInventory) calculateDiff(new, old map[string]interface{}) map[string]interface{} {
	motype, ok := new[adcore.BaseMoObjectTypePropName].(string)
	if !ok {
		return nil // Would be caught earlier
	}

	var out map[string]interface{}
	for k, v := range new {
		// Try fast path for primitive types, else default to reflection.
		switch v.(type) {
		case float64:
			ov, ok := old[k].(float64)
			if !ok || ov != v.(float64) {
				if out == nil {
					out = make(map[string]interface{})
				}
				out[k] = v
			}
		case string:
			ov, ok := old[k].(string)
			if !ok || ov != v.(string) {
				if out == nil {
					out = make(map[string]interface{})
				}
				out[k] = v
			}
		case bool:
			ov, ok := old[k].(bool)
			if !ok || ov != v.(bool) {
				if out == nil {
					out = make(map[string]interface{})
				}
				out[k] = v
			}
		case *adcore.MoRef:
			ov, ok := old[k].(*adcore.MoRef)
			if !ok || (ov.Selector != v.(*adcore.MoRef).Selector || ov.ObjectType != v.(*adcore.MoRef).ObjectType) {
				if out == nil {
					out = make(map[string]interface{})
				}
				out[k] = v
			}
		default:
			// TODO: Single level equality check done for now. Need recursive checks to allow for
			// TODO: sub-document diffs.
			// TODO: Profile this call and optimize away.
			if !reflect.DeepEqual(v, old[k]) {
				if out == nil {
					out = make(map[string]interface{})
				}
				out[k] = v
			}
		}
	}

	if out != nil {
		p.objectIdsMu.Lock()
		ids := p.objectIdentities[motype]
		p.objectIdsMu.Unlock()

		out[adcore.BaseMoObjectTypePropName] = motype
		for _, id := range ids {
			out[id] = new[id]
		}
	}
	return out
}

// Construct a key for the 'object' based on the identity fields defined in
// the Intersight model for the ObjectType
func (p *CommonInventory) constructKey(ctx context.Context, object map[string]interface{}, key *bytes.Buffer) error {
	key.Reset()

	motype, ok := object[adcore.BaseMoObjectTypePropName].(string)
	if !ok {
		return errors.New("missing field 'ObjectType'")
	}
	_, err := key.WriteString("ObjectType=")
	if err != nil {
		return err
	}
	_, err = key.WriteString(motype)
	if err != nil {
		return err
	}

	p.objectIdsMu.Lock()
	ids, ok := p.objectIdentities[motype]
	p.objectIdsMu.Unlock()
	if !ok {
		adlog.MustFromContext(ctx).Sugar().Infof("known ids: %v", p.objectIdentities)
		return fmt.Errorf("can't find identity for : %s", motype)
	}

	// Singleton object
	if len(ids) == 0 {
		return nil
	}

	for _, field := range ids {
		var valIf interface{}
		valIf, ok = object[field]
		if !ok {
			return fmt.Errorf("object is missing an identity field field=%v", field)
		}

		_, err = key.WriteString(":")
		if err != nil {
			return err
		}

		_, err = key.WriteString(field)
		if err != nil {
			return err
		}

		_, err = key.WriteString("=")
		if err != nil {
			return err
		}

		// todo: consider whether stringifying via fmt.Sprint is safe, ie whether
		//  it will be stable across golang versions and handle all value types
		_, err = key.WriteString(fmt.Sprint(valIf))
		if err != nil {
			return err
		}
	}

	return nil
}
