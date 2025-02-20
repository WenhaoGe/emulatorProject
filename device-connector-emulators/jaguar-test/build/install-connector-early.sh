#!/bin/bash
LOGGER() {
	/usr/bin/logger -t "install-connector" -p user.notice -- $1
}

EXECUTE() {
	if [ -e /etc/install.debug ]; then
		LOGGER "$1"
	else
		eval $1
	fi
	return $?
}
PATH=$PATH:/nuova/bin


ACTIVE_CLOUD_INDEX_FILE=/mnt/nand/ucs-mgnt-cloud-connector/active-go-img-index


usage ()
{
    echo "Usage: "
    echo "`basename $0` <filename>"
    echo "{\"ErrorCode\": \"INVALID_ARGUMENT_COUNT\", \"ErrorMsg\": \"Invalid input argument count\"}"
    rm -rf $1
    exit 1
}

if [ ! -e $FLASHCP ]; then
	echo "flashcp not found"
	echo "{\"ErrorCode\": \"SERVER_INTERNAL_ERROR\", \"ErrorMsg\": \"Server internal processing error\"}"
	rm -rf $1
	exit 1;
fi

#0. Input argument count validation
if [ $# -ne 1 ]; then
	usage
fi

#1. Check for any ongoing installation and lock it if not
if [ -f /var/run/bios-update.pid -o -f /var/run/bmc-download.pid -o -f /var/run/bmc-update.pid -o -f /var/run/pid-catalog-update.pid -o -f /var/run/sd-util-update.pid ]; then
	echo "{\"ErrorCode\": \"SERVER_BUSY_ERROR\", \"ErrorMsg\": \"Server busy in another installation\"}"
	rm -rf $1
	exit 1;
fi

mkdir -p /var/run
echo $$ > /var/run/bmc-update.pid

mkdir -p /opt/cloudimg
unsquashfs -d /opt/cloudimg -f $1

if [ -f /opt/cloudimg/cisco/scripts/install-connector-late.sh ]; then
	#Invoke install-connector-late.sh
	/opt/cloudimg/cisco/scripts/install-connector-late.sh > /dev/null
else
	#Finally start the device connector
	/etc/init.d/ucs-mgmt-cloud-connector start > /dev/null
fi

rm -rf /var/run/bmc-update.pid
exit $RET

