package xmlutils

import (
	"bytes"
	"database/sql"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/boltdb/bolt"
	"github.com/clbanning/mxj"
	_ "github.com/mattn/go-sqlite3" // MUST Uncomment to run this, removing as it fails UT
)

// Converts an xml file containing a hierarchical configResolve of the IMC topSystem object to
// a bolt database file with keys as dns and value as xml representation of mo.
func ConvertIMCXmlToBolt(file string) {
	mxj.XMLEscapeChars(true)

	f, err := os.Open(file)
	if err != nil {
		panic(err)
	}

	mv, err := mxj.NewMapXmlReader(f) // unmarshal
	if err != nil {
		panic(err)
	}

	// Add dn to all mos.
	addDn(mv, nil)

	opts := &bolt.Options{
		Timeout: 10 * time.Second,
	}

	boltdb, err := bolt.Open(strings.Split(file, ".")[0]+".bolt", 0600, opts)
	if err != nil {
		panic(err)
	}

	for k, v := range mv {
		if mv, ok := v.(map[string]interface{}); ok {
			writeMosToBolt(mv, k, boltdb)
		}
	}

	// Validate db contents
	err = boltdb.View(func(tx *bolt.Tx) error {
		// Assume bucket exists and has keys
		b := tx.Bucket([]byte(moxmlBucket))

		c := b.Cursor()

		for k, v := c.First(); k != nil; k, v = c.Next() {
			_, merr := mxj.NewMapXml(v)
			if merr != nil {
				fmt.Printf("failed parse of xml value: %s, %s", string(v), merr.Error())
				return merr
			}
		}

		return nil
	})

	if err != nil {
		panic(err)
	}
}

// Writes to bolt db the mo dn as key and xml as value.
func writeMosToBolt(m map[string]interface{}, className string, boltdb *bolt.DB) {
	// Make copy of input map and retain only attributes.
	newmap := make(map[string]interface{})
	for k, v := range m {
		if strings.HasPrefix(k, "-") {
			newmap[k] = v
		}
	}

	if className == "memoryArray" {
		fmt.Printf("mem array: %+v", m)
	}

	// Write trimmed mo to db, only if it has a dn.
	if dn, ok := newmap["-dn"].(string); ok {
		if dn == "" {
			panic("empty dn")
		}
		buf := bytes.NewBuffer(nil)

		xmlval, err := mxj.Map(newmap).Xml(className)
		if err != nil {
			panic(err)
		}

		buf.Write(xmlval)

		err = boltdb.Update(func(tx *bolt.Tx) error {
			b, berr := tx.CreateBucketIfNotExists([]byte("moxml"))
			if berr != nil {
				return fmt.Errorf("create bucket: %s", berr)
			}
			return b.Put([]byte(dn), buf.Bytes())
		})

		if err != nil {
			panic(err)
		}
	} else {
		fmt.Println("no dn found for class: ", className)
	}

	for k, v := range m {
		if mv, ok := v.(map[string]interface{}); ok {
			//fmt.Println("processing mo: ", k)
			writeMosToBolt(mv, k, boltdb)
		} else if mvs, ok := v.([]interface{}); ok {
			//panic("its a slice you dolt")
			for _, mv := range mvs {
				writeMosToBolt(mv.(map[string]interface{}), k, boltdb)
			}
		}
	}
}

func addDn(m map[string]interface{}, parent map[string]interface{}) {
	if parent != nil {
		if pdn, ok := parent["-dn"]; ok {
			if _, ok = m["-rn"]; !ok {
				newmap := make(map[string]interface{})
				for k, v := range m {
					if strings.HasPrefix(k, "-") {
						newmap[k] = v
					}
				}
				fmt.Printf("no rn found on this thing: %+v\n", newmap)
			} else {
				m["-dn"] = pdn.(string) + "/" + m["-rn"].(string)
				//fmt.Printf("added dn: %s\n", m["-dn"].(string))
			}
		} else {
			newmap := make(map[string]interface{})
			for k, v := range parent {
				if strings.HasPrefix(k, "-") {
					newmap[k] = v
				}
			}
			newmap2 := make(map[string]interface{})
			for k, v := range m {
				if strings.HasPrefix(k, "-") {
					newmap2[k] = v
				}
			}
			fmt.Printf("no dn found on parent: %+v\n for child: %+v\n", newmap, newmap2)
		}
	}

	for k, v := range m {
		if !strings.HasPrefix(k, "-") {
			if mv, ok := v.(map[string]interface{}); ok {
				addDn(mv, m)
			} else if mvs, ok := v.([]interface{}); ok {
				for _, mv := range mvs {
					addDn(mv.(map[string]interface{}), m)
				}
			} else {
				panic("unknown sub type")
			}
		}
	}
}

const moxmlBucket = "moxml"

const classQuery = "select * from modb where moxml like ?"

// Converts a UCSM dme sqlite database file to
// a bolt database file with keys as dns and value as xml representation of mo.
func ConvertSamDmeToBolt(dbfile string, boltdbfile string) {
	sqldb, err := sql.Open("sqlite3", dbfile)
	if err != nil {
		fmt.Println("failed to open dme db: ", err.Error())
		panic(err)
	}
	fmt.Println("sqlite db opened")

	opts := &bolt.Options{
		Timeout: 10 * time.Second,
	}

	boltdb, err := bolt.Open(boltdbfile, 0600, opts)
	if err != nil {
		fmt.Println("failed to open bolt db: ", err.Error())
		panic(err)
	}

	err = boltdb.Update(func(tx *bolt.Tx) error {
		_, berr := tx.CreateBucketIfNotExists([]byte(moxmlBucket))
		if berr != nil {
			return fmt.Errorf("create bucket: %s", berr)
		}
		return nil
	})

	if err != nil {
		panic(err)
	}

	classNameInput := " <%"

	rows, err := sqldb.Query(classQuery, &classNameInput)
	if err != nil {
		fmt.Println("failed db query: ", err.Error())
		panic(err)
	}

	var key string
	var moxml []byte
	var checksum int

	for rows.Next() {
		err = rows.Scan(&key, &moxml, &checksum)
		if err != nil {
			fmt.Println("scan error: ", err.Error())
			panic(err)
		}

		xmlm, err := mxj.NewMapXml(moxml)
		if err != nil {
			panic(err)
		}

		//fmt.Println("xml object: %+v", xmlm)

		if len(xmlm) == 1 {
			for k, v := range xmlm {
				if _, ok := ucsmWhitelist[k]; !ok {
					continue
				}
				mv := v.(map[string]interface{})
				if dn, ok := mv["-dn"].(string); ok {
					err = boltdb.Update(func(tx *bolt.Tx) error {
						b := tx.Bucket([]byte(moxmlBucket))

						if b == nil {
							fmt.Println("moxml bucket not exist")
							return nil
						}

						for k, v := range mv {
							if strings.Contains(k, "Time") || strings.Contains(k, "created") || strings.Contains(k, "lastTransition") {
								sv, ok := v.(string)
								if !ok {
									fmt.Printf("%v / %T is not a string time\n", v, v)
									continue
								}

								fv, err := strconv.ParseFloat(sv, 64)
								if err != nil {
									continue
								}

								mv[k] = time.Unix(int64(math.Floor(fv)), 0).UTC().Format("2006-01-02T15:04:05.000")
								fmt.Printf("converted time : %s on %s\n", mv[k], dn)
								//fmt.Printf("converted mo : %+v\n", mv)
								fmt.Printf("pre convert : %s\n", string(moxml))
								moxml = bytes.Replace(moxml, []byte(sv), []byte(mv[k].(string)), -1)
								fmt.Printf("post convert : %s\n", string(moxml))

							}
						}

						return b.Put([]byte(dn), moxml)

					})
					if err != nil {
						panic(err)
					}
				} else {
					fmt.Println("no dn found for object: ", string(moxml))
				}
			}
		} else {
			fmt.Println("more than one xml object in row: ", string(moxml))
		}
	}

}

// Whitelist of ucsm objects Intersight is interested in. Used to reduce db size.
// Revisit if more mos are added or all objects are needed.
// Taken from generated constant file:
// https://bitbucket-eng-sjc1.cisco.com/bitbucket/projects/AN/repos/dejavu/browse/utils
var ucsmWhitelist = map[string][]string{
	"adaptorUnit":                 []string{"vendor", "model", "serial", "revision", "id", "partNumber", "operability", "operState", "baseMac", "power", "thermal", "presence", "integrated", "pciSlot", "vid"},
	"adaptorExtEthIf":             []string{"id", "operState", "adminState", "peerDn", "epDn", "mac", "ifType"},
	"adaptorHostEthIf":            []string{"vendor", "model", "serial", "revision", "id", "name", "operState", "operability", "adminState", "peerDn", "epDn", "vnicDn", "mac", "originalMac", "pciAddr", "virtualizationPreference", "ifType"},
	"adaptorHostFcIf":             []string{"vendor", "model", "serial", "revision", "id", "name", "adminState", "operState", "operability", "peerDn", "epDn", "wwn", "nodeWwn", "originalWwn", "originalNodeWwn"},
	"adaptorHostIscsiIf":          []string{"vendor", "model", "serial", "revision", "id", "name", "adminState", "operState", "peerDn", "epDn", "operability", "mac", "hostVisible"},
	"biosUnit":                    []string{"vendor", "model", "serial", "revision", "initSeq", "initTs"},
	"computeRackUnit":             []string{"vendor", "model", "serial", "revision", "adminPower", "operPower", "operState", "operability", "uuid", "presence", "totalMemory", "memorySpeed", "availableMemory", "numOfCpus", "numOfCores", "numOfCoresEnabled", "numOfThreads", "numOfAdaptors", "numOfEthHostIfs", "numOfFcHostIfs", "assignedToDn", "assetTag", "usrLbl", "id"},
	"computeBlade":                []string{"vendor", "model", "serial", "revision", "adminPower", "operPower", "operState", "operability", "uuid", "presence", "totalMemory", "memorySpeed", "availableMemory", "numOfCpus", "numOfCores", "numOfCoresEnabled", "numOfThreads", "numOfAdaptors", "numOfEthHostIfs", "numOfFcHostIfs", "assignedToDn", "assetTag", "usrLbl", "chassisId", "slotId", "scaledMode"},
	"computeBoard":                []string{"vendor", "model", "serial", "revision", "id", "cpuTypeDescription", "presence", "power"},
	"equipmentSwitchCard":         []string{"vendor", "model", "serial", "revision", "descr", "numPorts", "presence", "state", "id"},
	"equipmentChassis":            []string{"vendor", "model", "serial", "revision", "operState"},
	"equipmentIOCard":             []string{"vendor", "model", "serial", "revision", "operState"},
	"equipmentFex":                []string{"vendor", "model", "serial", "revision", "operState"},
	"equipmentSystemIOController": []string{"vendor", "model", "serial", "revision", "operState"},
	"equipmentPsu":                []string{"vendor", "model", "serial", "revision", "id", "operState", "presence"},
	"equipmentFanModule":          []string{"vendor", "model", "serial", "revision", "operState", "presence"},
	"equipmentFan":                []string{"vendor", "model", "serial", "revision", "operState", "presence"},
	"equipmentLocatorLed":         []string{"operState", "color"},
	"equipmentSlotEp":             []string{"vendor", "model", "serial", "revision", "refDn", "id"},
	"equipmentRackEnclosure":      []string{"vendor", "model", "serial", "revision", "id"},
	"equipmentIOExpander":         []string{"vendor", "model", "serial", "revision", "operState", "presence"},
	"equipmentTpm":                []string{"vendor", "model", "serial", "revision", "id", "presence", "ownership", "activeStatus", "enabledStatus", "tpmRevision"},
	"etherPIo":                    []string{"ifRole", "operState", "mac", "xcvrType"},
	"faultInst":                   []string{"descr", "code", "severity", "origSeverity", "prevSeverity", "rule", "created", "lastTransition", "occur", "ack", "dn"},
	"fcPIo":                       []string{"ifRole", "operState", "wwn", "xcvrType"},
	"firmwareRunning":             []string{"deployment", "type", "version", "packageVersion"},
	"graphicsCard":                []string{"vendor", "model", "serial", "revision", "deviceId", "expanderSlot", "firmwareVersion", "id", "mode", "operState", "pciAddr", "pciAddrList", "slotName"},
	"graphicsController":          []string{"vendor", "model", "serial", "revision", "id", "pciAddr", "pciSlot"},
	"moKvInvHolder":               []string{"endpoint"},
	"moInvKv":                     []string{"key", "value", "type"},
	"lsServer":                    []string{"name", "pnDn", "assocState", "assignState", "configState", "operState"},
	"mgmtController":              []string{"model"},
	"mgmtIf":                      []string{"extIp", "extGw", "extMask", "mac"},
	"mgmtEntity":                  []string{"leadership", "id"},
	"memoryArray":                 []string{"vendor", "model", "serial", "revision", "id", "cpuId", "maxDevices", "maxCapacity", "currCapacity", "errorCorrection", "power", "presence"},
	"memoryUnit":                  []string{"vendor", "model", "serial", "revision", "id", "type", "array", "bank", "capacity", "clock", "speed", "formFactor", "latency", "location", "visibility", "width", "set", "adminState", "operability", "operState", "power", "thermal", "presence"},
	"topSystem":                   []string{"mode", "name", "address", "ipv6Addr"},
	"networkElement":              []string{"vendor", "model", "serial", "revision", "id", "adminInbandIfState", "inbandIfIp", "inbandIfGw", "inbandIfMask", "inbandIfVnet", "oobIfIp", "oobIfGw", "oobIfMask", "oobIfMac"},
	"pciEquipSlot":                []string{"vendor", "model", "serial", "revision", "hostReported"},
	"pciSwitch":                   []string{"vendor", "model", "serial", "revision", "pciSlot", "pciAddr", "productName", "productRevision", "temperature", "deviceId", "vendorId", "subDeviceId", "subVendorId", "health", "numOfAdaptors"},
	"pciLink":                     []string{"vendor", "model", "serial", "revision", "adapter", "pciSlot", "linkStatus", "linkSpeed", "linkWidth", "slotStatus"},
	"coprocessorCard":             []string{"vendor", "model", "serial", "revision", "id", "pciSlot"},
	"portGroup":                   []string{"transport"},
	"portSubGroup":                []string{"transport"},
	"processorUnit":               []string{"vendor", "model", "serial", "revision", "id", "arch", "socketDesignation", "cores", "coresEnabled", "threads", "stepping", "speed", "operability", "operState", "power", "thermal", "presence"},
	"securityUnit":                []string{"vendor", "model", "serial", "revision", "id", "operState", "operability", "power", "presence", "voltage", "thermal", "vid", "partNumber", "pciSlot"},
	"storageController":           []string{"vendor", "model", "serial", "revision", "id", "controllerFlags", "controllerStatus", "operState", "operability", "hwRevision", "pciAddr", "pciSlot", "raidSupport", "oobInterfaceSupported", "rebuildRate", "presence", "type"},
	"storageLocalDisk":            []string{"vendor", "model", "serial", "revision", "id", "size", "rawSize", "blockSize", "physicalBlockSize", "numberOfBlocks", "deviceType", "variantType", "connectionProtocol", "linkSpeed", "linkState", "bootable", "configState", "configCheckPoint", "discoveredPath", "diskState", "driveState", "presence", "powerState", "thermal", "operability", "operQualifierReason"},
	"storageVirtualDrive":         []string{"vendor", "model", "serial", "revision", "accessPolicy", "actualWriteCachePolicy", "availableSize", "blockSize", "bootable", "configState", "configuredWriteCachePolicy", "connectionProtocol", "driveCache", "driveSecurity", "driveState", "id", "ioPolicy", "name", "numberOfBlocks", "operState", "operability", "physicalBlockSize", "presence", "readPolicy", "securityFlags", "size", "stripSize", "type", "uuid", "vendorUuid"},
	"storageVDMemberEp":           []string{"id", "spanId", "role", "presence", "operQualifierReason"},
	"storageFlexFlashController":  []string{"vendor", "model", "serial", "revision", "id", "controllerState"},
	"storageSasExpander":          []string{"vendor", "model", "serial", "revision", "id", "operState", "operability", "presence"},
	"storageEnclosure":            []string{"vendor", "model", "serial", "revision", "id", "descr", "chassisId", "serverId", "type", "numSlots", "presence"},
	"storageLocalDiskEp":          []string{"vendor", "model", "serial", "revision", "id", "bootable", "diskState", "diskDn"},
	"storageVirtualDriveEp":       []string{"id", "bootable", "driveState", "vdDn", "name", "containerId", "operDeviceId", "uuid", "vendorUuid"},
}
