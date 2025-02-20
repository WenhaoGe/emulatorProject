#!/bin/bash
nohup python /route.py &
nohup python /identity.py &
nginx -c /nginx.conf
python /setupdb.py
#/nuova/bin/install-connector-early.sh /mnt/scratchpad/cloudimgfs.img
sleep 5
nohup /opt/cloudimg/cisco/bin/ucs_mgmt_cloud_connector --log_path="/var/log/dc/dc.log" --dbdir=/opt/db/ &
for (( ; ; ))
do
    sleep 100
done
