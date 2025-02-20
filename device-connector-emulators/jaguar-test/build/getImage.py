#import docker
import sys
import os
import errno
import requests
import urllib
from requests.auth import HTTPBasicAuth
from socket import error as SocketError
import time

def downloadFile(downloadUrl, fileName):
    numRetries = 5
    retryCount = 0
    RETRYINTERVAL_SEC = 2
    print("downloading {} to fileName {}".format(downloadUrl, fileName))
    while True:
        if retryCount >= numRetries:
            print("retryCount: {}, numRetries: {}".format(retryCount, numRetries))
            raise Exception("unable to download {}/{}".format(downloadUrl,fileName))

        #https://stackoverflow.com/questions/20568216/python-handling-socket-error-errno-104-connection-reset-by-peer
        #sometimes we get 'Connection reset by peer' while downloading files
        #this code retries it 'numRetries' times
        try:
            urllib.urlretrieve(downloadUrl,fileName)
            break
        except SocketError as e:
            if e.errno != errno.ECONNRESET:
                raise
            print("{}: Caught exception {}. Retrying".format(retryCount, e))

        print("dowload failed. Retrying after {} seconds".format(RETRYINTERVAL_SEC))
        time.sleep(RETRYINTERVAL_SEC)
        retryCount += 1

def getJaguarImage(relType, version, username, password):
    aqlFile = ''
    name = ""
    path = ""
    repo = ""
    if version == 'latest':
        if relType == "release":
            aqlFile = 'image_aql.release'
        elif relType == "debug":
            aqlFile = 'image_aql.debug'
        else:
            print("Invalid release type")
            sys.exit(2)

        aql = open(aqlFile).read()

        url = 'https://engci-maven-master.cisco.com/artifactory/api/search/aql'
        req = requests.post(url, data = aql, auth=HTTPBasicAuth(username, password))
        print("status: ", req.status_code)
        out = req.json()
        print(out)

        name = out['results'][0]['name']
        path = out['results'][0]['path']
        repo = out['results'][0]['repo']

    else:
        name = 'ucsc-m5-cimc-cloud-connector-'+version+'.bin'
        path = 'andromeda/jaguar/'+ version.split('-')[0] + '/' + version
        if relType == 'release':
            repo = 'cspg-andromeda-release'
        else:
            repo = 'cspg-andromeda-snapshot'
    print("Name: ", name)
    print("Path: ", path)
    print("Repo: ", repo)

    downloadUrl = "https://engci-maven-master.cisco.com/artifactory/"+repo+"/"+path+"/"+name
    fileName = 'ucsc-connector.bin'
    downloadFile(downloadUrl, fileName)
    imgVersion = path.split('/')[-1]
    return imgVersion
