import requests
import argparse
import os
import json


parser = argparse.ArgumentParser(description='Get running emulators claim info')
parser.add_argument('directory', help='directory where emulators are running')


args = parser.parse_args()

claims = {}

for p in os.listdir(args.directory):
    try:
        req = requests.get("http://localhost:"+str(p)+"/DeviceIdentifiers", headers={'ucsm-privs' : 'admin'})
        if req.status_code != 200:
            print(str(p)+":requests failed:", req.status_code)
            continue
    except:
        continue
    resp = req.json()
    id = resp[0]['Id']
    try:
        req = requests.get("http://localhost:"+str(p)+"/SecurityTokens", headers={'ucsm-privs' : 'admin'})
    except:
        continue
    if req.status_code != 200:
        print(str(p)+":requests failed:", req.status_code)
        continue
    resp = req.json()
    token = resp[0]['Token']
    claims[str(id)] = str(token)

print(json.dumps(claims))
