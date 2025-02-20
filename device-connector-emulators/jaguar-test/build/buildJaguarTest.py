#!/usr/bin/env python

#import docker

import argparse
import getpass
import os
import subprocess

from getImage import getJaguarImage


cec_user = os.getenv('CEC_USER')
cec_pass = os.getenv('CEC_PASS')

if not cec_user:
    cec_user = raw_input('Please enter your cec username: ')

if not cec_pass:
    cec_pass = getpass.getpass('Please enter your cec password: ')


parser = argparse.ArgumentParser()
parser.add_argument('--type', default='release')
parser.add_argument('--version', default='latest')
parser.add_argument('--push', nargs='?', const="")


args = parser.parse_args()
print(args)

version = getJaguarImage(args.type, args.version, cec_user, cec_pass)

p = subprocess.Popen(['docker', 'build', '-t', 'dockerhub.cisco.com/cspg-docker/andromeda/jaguar:'+version, '.'])
p.communicate()

if args.push == "":
    p = subprocess.Popen(['docker', 'push', 'dockerhub.cisco.com/cspg-docker/andromeda/jaguar:'+version])
    p.communicate()
