#import docker
import subprocess
import os
import argparse
import requests
import json
from pprint import pprint
import time

import socket
from contextlib import closing

def find_free_port():
    with closing(socket.socket(socket.AF_INET, socket.SOCK_STREAM)) as s:
        s.bind(('', 0))
        return s.getsockname()[1]

# Tested with python 3.5.0

# Uses docker to run a number of connector containers
# Runs all containers detached.
# To clean up for now use: (CAUTION: will remove ALL running containers)
# docker rm -f $(docker ps -a -q)


dockerCmd = 'docker'

parser = argparse.ArgumentParser(description='Start a number of connector containers')
parser.add_argument('numberContainers', type=int, help='number of containers to spawn')
parser.add_argument('--version', default="latest", help="version of the device connector to run")
parser.add_argument('--startingPort', type=int, help='starting host port to map to')
parser.add_argument('--persistLocation', nargs='?', help='directory to store connector db/logs', const="")
parser.add_argument('--onprem', nargs='?', const="", help='on prem ip')
parser.add_argument('--cloud', nargs='?', const="", help='cloud to register to')
parser.add_argument('--pull', nargs='?', const="", help='pull latest from dockerhub')
parser.add_argument('--nameprefix', help='prefix to assign container')

args = parser.parse_args()

imgNum = args.numberContainers
onpremIp = args.onprem


defaultConfig = json.load(open('../../commonScripts/connector.db'))

if args.cloud != None:
    defaultConfig['CloudDns'] = args.cloud

if args.persistLocation is None:
    persDir = '/tmp/jaguar-test/'
else:
    print(args.persistLocation)
    persDir = args.persistLocation

namePrefix = 'dcemul'
if not args.nameprefix is None:
    namePrefix = args.nameprefix

#pprint(json.dumps(defaultConfig))
jaguarTest = 'dockerhub.cisco.com/cspg-docker/andromeda/jaguar:' + args.version
if not args.pull is None:
    print("pulling latest")
    p = subprocess.Popen([dockerCmd, 'pull', jaguarTest])
    p.communicate()

for i in range(0, imgNum):
    port = 0
    if args.startingPort is None:
        port = find_free_port()
    else:
        port = args.startingPort + i
    print('using port:', port)
    if not os.path.exists(persDir+str(port)):
                os.makedirs(persDir+str(port))
                os.makedirs(persDir+str(port)+'/db')
                os.makedirs(persDir+str(port)+'/logs')
                with open(persDir+str(port)+'/db/connector.db', 'w') as dbfile:
                        json.dump(defaultConfig, dbfile)

    ip = str(i) + "." + str(i) + "." + str(i) + "." + str(i)
    hostname = namePrefix + str(port)

    runArgs =[dockerCmd, 'run']
    runArgs.append('-d')
    if not args.onprem is None:
        runArgs.extend(['--add-host', 'service-elb.andromdeda.cisco.com:'+args.onprem])
    runArgs.extend(['-e', 'ENV_IP='+ip, '-e', 'ENV_HOSTNAME='+hostname])
    runArgs.extend(['-v', persDir+str(port)+'/db:/opt/db', '-v', persDir+str(port)+'/logs:/var/log/dc'])
    runArgs.extend(['-p', str(port)+':80'])
    runArgs.extend(['--name', hostname])
    runArgs.extend([jaguarTest])
    print(runArgs)
    p = subprocess.Popen(runArgs)
    p.communicate()

