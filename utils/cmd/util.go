package main

import (
	"os"
	"path"
	"strings"

	xmlutils "bitbucket-eng-sjc1.cisco.com/an/apollo-test/utils/xml"
)

func main() {
	dbtype := path.Ext(os.Args[1])
	switch dbtype {
	case ".xml":
		xmlutils.ConvertIMCXmlToBolt(os.Args[1])
	case ".sqlite":
		xmlutils.ConvertSamDmeToBolt(os.Args[1], strings.Split(os.Args[1], ".")[0]+".bolt")
	default:
		panic("unknown db type: " + dbtype)
	}
}
