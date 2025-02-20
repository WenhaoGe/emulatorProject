package main

import (
	"flag"
	"fmt"

	"bitbucket-eng-sjc1.cisco.com/an/apollo-test/dcSpewer/apiServer"
	"bitbucket-eng-sjc1.cisco.com/an/apollo-test/dcSpewer/emulatorConnection"
)

func main() {
	if err := flag.Set("log_path", ""); err != nil {
		panic(err)
	}

	fmt.Println("Starting")

	apiServer.Start()

	emulatorConnection.StartEmulators()

	<-make(chan struct{})

}
