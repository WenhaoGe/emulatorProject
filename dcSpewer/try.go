package main

import (
	"fmt"
	"strconv"
)

func main() {

	fexId := 1
	iomId := 1
	portId := 30

	dn := "chassis-" + strconv.Itoa(fexId) + "-ioc-" + strconv.Itoa(iomId) + "-muxhostport-port-" + strconv.Itoa(portId)
	another := fmt.Sprintf("chassis-%d-ioc-%d-muxhostport-port-%d", fexId, iomId, portId)
	fmt.Println(another)
	fmt.Println(dn)

	// var a bool
	// fmt.Print(a)
	//
	// a = true
	// if !a {
	// 	fmt.Println(0)
	// } else {
	// 	fmt.Println(1)
	// }
}
