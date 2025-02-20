### Redfish emulator

The codebase for this emulator comes from [DMTF Redfish Mock Server](https://github.com/DMTF/Redfish-Mockup-Server.git)

#### Creating emulator data mocks

Creator is a modified [DMTF's Redfish Mock Creator](https://github.com/DMTF/Redfish-Mockup-Creator.git) code to accomodate chassis/blade inventory collection.

The process creates a mock folder structure from live redfish API.

* cd into [mock-creator](https://bitbucket-eng-sjc1.cisco.com/bitbucket/projects/AN/repos/apollo-test/browse/redfish-emulator/mock-creator)
* Execute mock creator to create datasource: `python redfishMockupCreate.py -r <server_ip> -u <user> -p <password> -D <path-to-inventory-folder>/<SERVER_PID>`
* To generate inventory fo chassis pass `-F` to the command. 
	*By default inventory for Chassis 1 is gathered. To change chassis ID set environment variable `CHASSIS_ID` to desired ID*  
    Example: `CHASSIS_ID=<chassis_id> python redfishMockupCreate.py -r <fi_ip> -u adminbackup -p <password> -D <path-to-inventory-folder>/<SERVER_PID> -F`
* To generate inventory for blade pass `-F` to the command and set `BLADE_IP` envornment variable with blade's IPv6 (displayed on blade's device registration page)  
	Example: `BLADE_IP=<blade_ipv6> python redfishMockupCreate.py -r <fi_ip> -u adminbackup -p <password> -D <path-to-inventory-folder>/<SERVER_PID> -F`

*Note: Inventory folder can either be <path-to-apollo-test-repo>/redfish-emulator/src/mocks or local path that is mounted to the container.*


#### Docker image

Build image by running: `docker build -t dockerhub.cisco.com/cspg-docker/andromeda/apollo-test/redfish:<version> .`

The container exposes redfish API on port 8000.

When running the image `mocks/` directory can be mounted to the container at
`/app/mocks`. If
the plan is to run the image without providing `SERVER_PID` environment
variable, make sure `mocks/default` directory exists with mock redfish files.

