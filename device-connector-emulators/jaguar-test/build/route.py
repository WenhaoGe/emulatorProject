#!/usr/bin/env python
import os
from flask import Flask, request, Response
from xml.etree import ElementTree

import logging
import socket
import ConfigParser

from emulator import CimcEmulator
from faultinjector import FaultInjector

LOG_FILENAME = '/var/log/emulator.log'
logging.basicConfig(format='%(asctime)s:%(message)s' , datefmt='%m/%d/%Y %I:%M:%S %p', filename=LOG_FILENAME, level=logging.DEBUG)

class xml_response(Response):
    default_mimetype = 'application/xml'

app = Flask(__name__)
app.response_class = xml_response

# Read the input configuation from rack.cfg
config = ConfigParser.RawConfigParser()
config.read('rack.cfg')
cimc_ip = config.get('emulate', 'ip')
cimc_user = config.get('emulate', 'username')
cimc_password = config.get('emulate', 'password')
cimc_port = config.get('emulate', 'port')
cimc_model = os.getenv('IMC_MODEL', "UCSC-C220-M5L")
emulator_port = config.get('emulator', 'port')
emulator_ip = ([(s.connect(('8.8.8.8', 53)), s.getsockname()[0], s.close()) for s in [socket.socket(socket.AF_INET, socket.SOCK_DGRAM)]][0][1])

emulator = CimcEmulator(cimc_model, emulator_ip, emulator_port)
emulator.start(cimc_ip, cimc_port, cimc_user, cimc_password)

faultInjector = FaultInjector("./faults", emulator)
faultInjector.start()

filename = "./faults/fault_injector.xml"
if os.path.exists(filename):
    with open(filename, "a") as sfile:
        sfile.write(" ")
        sfile.close()

@app.before_request
def before_request():
    if True:
        print("HEADERS", request.headers)
        print("REQ_path", request.path)
        print("ARGS",request.args)

@app.route('/nuova', methods=['GET', 'POST'])
@app.route('/cloud', methods=['GET', 'POST'])
def nuova():
    request_data = ""
    if request.mimetype == "application/x-www-form-urlencoded":
        keys = request.form.keys()
        for each in keys:
            request_data += each + "=" + request.form[each]
    elif request.mimetype == "text/xml":
        request_data = request.data
    else:
        print("Unhandled mimetype:", request.mimetype)
    tree = ElementTree.fromstring(request_data)

    # Search for the login method and get the username and password

    logging.debug("Incoming payload : " + request_data)
    return emulator.handle_request(tree)


@app.route('/data/login', methods=['GET', 'POST'])
def sel_login():
    request_data = request.stream.read()
    print("SEL Login", request_data)
    return emulator.handle_sel_login(request_data)

@app.route('/data/logout', methods=['GET', 'POST'])
def sel_logout():
    request_data = request.stream.read()
    print("SEL Logout", request_data)
    return emulator.handle_sel_logout(request_data)

@app.route('/data', methods=['GET', 'POST'])
def sel_log():
    request_data = request.stream.read()
    print("SEL requested", request_data)
    return emulator.handle_sel_request(request_data)


if __name__ == '__main__':
    app.run(host='0.0.0.0', port=int(emulator_port))
