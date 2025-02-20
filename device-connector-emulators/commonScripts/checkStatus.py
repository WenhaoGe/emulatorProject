import requests
import argparse
import os
import json

parser = argparse.ArgumentParser(description='Remove emulators running on a list of ports')
parser.add_argument('directory', help='directory where emulators are running')


args = parser.parse_args()

statuses = []
for p in os.listdir(args.directory):
    try:
        req = requests.get("http://localhost:"+str(p)+"/Systems", headers={'ucsm-privs' : 'admin'})
    except Exception as e:
        print("request failed", p, e)
        continue
    if req.status_code != 200:
        print("requests failed:", req.status_code, p)
        continue
    resp = req.json()
    statuses.append(resp[0]['ConnectionState'])
    print("good", p)
    #statuses.append(resp[0]['AccountOwnershipUser'])
    #statuses.append(resp[0]['Version'])
    #print("Connection status of container on port", p, ":", resp[0]['ConnectionState'])

out = json.dumps(statuses)
print(out)
print(len(statuses))
