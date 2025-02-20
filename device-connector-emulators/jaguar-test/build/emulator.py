#!/usr/bin/env python
from errno import EEXIST
from time import sleep
from xml.etree import ElementTree
from xml.etree.ElementTree import Element

from imcsdk.imchandle import ImcHandle
import logging
import os
import random
import string
import re

LOG_FILENAME = './var/log/emulator.log'

CONFIGCONF_RESPONSE_DELAY={}
CONFIGRESOLVEDN_RESPONSE_DELAY={}
CONFIGRESOLVECLASS_RESPONSE_DELAY={}

logging.basicConfig(format='%(asctime)s:%(message)s' , datefmt='%m/%d/%Y %I:%M:%S %p', filename=LOG_FILENAME, level=logging.DEBUG)



# Create a serial number and persist across container restarts
def get_serial():
    logging.debug(os.path)

    filename = "/db/serial"
    if not os.path.exists(filename):
        #create db directory if it doesn't exist
        try:
            os.makedirs(os.path.dirname(filename))
        except OSError as ex:
            if ex.errno != EEXIST:
                raise

        sfile = open('/db/serial', 'w')
        serial = 'SS' + ''.join(random.choice(string.ascii_uppercase + string.digits) for _ in range(9))
        sfile.write(serial)
        sfile.close()
    else:
        sfile = open('/db/serial', 'r')
        serial = sfile.read()
        sfile.close()
    return serial


class CimcEmulator(object):
    def __init__(self, model, ip, port):
        """ Initializes the emulator based on model """
        self._model = model
        self._ip = ip
        self._port = port

    def mimic_cimc(self, cimc_ip, cimc_port, cimc_user, cimc_passwd):
        emulator_dump = self._model + '.xml'
        logging.debug("No config file, collecting data, mimic cimc")
        xml_dump_f = open(emulator_dump, "w")
        temp_handle = ImcHandle(cimc_ip, cimc_user, cimc_passwd)
        temp_handle.login(auto_refresh=False)

        # TODO: Fix this - doesnt work the same as a complete XML dump taken from curl
        emulator_mos = temp_handle.query_classid("topSystem", hierarchy=True)
        import imcsdk.imcxmlcodec as xc
        for mo in emulator_mos:
            xml_obj = mo.to_xml()
            xml_str = xc.to_xml_str(xml_obj)
            xml_dump_f.write(xml_str)
            xml_dump_f.close()
        self._rack_mos = emulator_mos

    def start(self, cimc_ip=None, cimc_port=None, cimc_user=None, cimc_passwd=None):

        """ Loads up the XML MOs and starts responding to incoming requests """
        import os.path
        emulator_dump = self._model + '.xml'
        if os.path.isfile(emulator_dump):
            logging.debug("config file exists")
            self._rack_mos = ElementTree.parse(emulator_dump)
        elif (cimc_ip) and (cimc_port) and (cimc_user) and (cimc_passwd):
            self.mimic_cimc(cimc_ip, cimc_port, cimc_user, cimc_passwd)
        else:
            logging.debug("Not enough to start, die!!")
            return
        # TODO: Defensively check if we do have rack_mos populated at this time

        self.build_dn_for_emulator()



        # for p in g.rack_mos.iter():
        # logging.debug(p.attrib)
        # TODO: Get the running firmware actually from the XML
        # TODO: Cycle between 4 sessions and cookies and validate incoming requests
        # TODO: Fix current time

    # Create a parent child mapping to help form the DN
    def build_dn_for_emulator(self):
        self._parent_map = {}
        for p in self._rack_mos.iter():
            if p.tag == "outConfigs":
                continue
            #Replace mgmtIf IPs
            elif p.tag == "topSystem":
                p.attrib["address"] = self._ip
            elif p.tag == "mgmtIf":
                p.attrib["extIp"] = self._ip
            # logging.debug(p.tag)
            for c in p:
                if c in self._parent_map:
                    self._parent_map[c].append(p)
                    # Or raise, if you don't want to allow this.
                else:
                    # logging.debug(c.attrib)
                    self._parent_map[c] = [p]
                    # logging.debug("**** rn = " + c.attrib["rn"] )
                    # logging.debug("**** dn = " + p.attrib["dn"] + "/" + c.attrib["rn"] )
                    c.set("dn", p.attrib["dn"] + "/" + c.attrib["rn"])
                    # Or parent_map[c] = p if you don't want to allow this

    def stop_emulator(self):
        """ How do we stop? """

    def change_config(self):
        """ Change MO states run time, without an incoming set API, allows to mimic Fault injection """

    def _handle_aaaLogin(self, login_elem):
        logging.debug("Login found")
        return '<aaaLogin cookie="" response="yes" outCookie="1486310032/85846ec0-47ca-17ca-9ab2-d2262cf6697c" outRefreshPeriod="600" outPriv="admin" outSessionId="7029" outVersion="1.5(4)"> </aaaLogin> '

    def _handle_aaaLogout(self, logout_elem):
        logging.debug("Logout found")
        return '<aaaLogout cookie="" response="yes" outStatus="success"> </aaaLogout>'

    def _handle_config_resolve_class(self, class_elem):
        logging.debug("CRC received")
        logging.debug(class_elem.attrib["classId"])
        logging.debug(class_elem.attrib["inHierarchical"])
        class_elem.attrib["cookie"] = "abcd/abcd"
        # logging.debug(g.imc_handle)
        logging.debug(self._rack_mos)
        mo_found = False
        logging.debug(os.path)
        logging.debug("Os real path : " + os.path.realpath('/db/serial'))
        serial = get_serial()
        out_xml = '<configResolveClass '
        out_xml += 'cookie="' + class_elem.attrib["cookie"] + '" response="yes" classId="' + class_elem.attrib["classId"]
        out_xml += '"> <outConfigs> '

        for mo in self._rack_mos.iter(tag=class_elem.attrib["classId"]):
            # logging.debug("mo found")
            mo_found = True
            if class_elem.attrib["inHierarchical"] == "True" or \
                    class_elem.attrib["inHierarchical"] == "true":
                # logging.debug(class_elem.attrib["classId"] + "Found")
                out_xml += ElementTree.tostring(mo)
            else:
                # Form a new tree with just the node and none of its children and return that
                out_mo = Element(mo.tag)
                for attr, value in mo.items():
                    out_mo.set(attr, value)
                out_xml += ElementTree.tostring(out_mo)
        out_xml += '</outConfigs> </configResolveClass>'
        #logging.debug(out_xml)   --- uncomment this to write the response back to the emulator.log
        out_xml = re.sub(r'serial=(".*")', 'serial="'+serial+'"', out_xml)
        logging.debug(out_xml)
        #TODO: Hack this for now, need to respond with the right error for invalid class versus empty, for now returning only for networkElement for error
        if not mo_found and class_elem.attrib["classId"] == 'networkElement':
            out_xml = '<error cookie="" response="yes" errorCode="ERR-xml-parse-error" invocationResult="594" errorDescr="XML PARSING ERROR: no class named' + class_elem.attrib["classId"] + '" />'

        # Adding delay for configResolveClass API call for a classId based on details given in faults/fault_injector.xml file.
        if CONFIGRESOLVECLASS_RESPONSE_DELAY:
            if CONFIGRESOLVECLASS_RESPONSE_DELAY.has_key(class_elem.attrib["classId"]):
                sleep(CONFIGRESOLVECLASS_RESPONSE_DELAY[class_elem.attrib["classId"]])
        return out_xml

    def _handle_config_resolve_children(self, dn_elem):
        logging.debug("CRChildren received")
        logging.debug(dn_elem.attrib["inDn"])
        logging.debug(dn_elem.attrib["inHierarchical"])

        if "cookie" not in dn_elem.attrib:
            dn_elem.attrib["cookie"] = "abcd/abcd"

        if "classId" in dn_elem.attrib:
            classID = dn_elem.attrib["classId"]

        out_xml = '<configResolveChildren cookie="' + dn_elem.attrib["cookie"] + '" response="yes" inDn="' + dn_elem.attrib[
            "inDn"] + '"> <outConfigs> '

        dn = ".//*[@dn='" + dn_elem.attrib["inDn"] + "']"
        logging.debug("Dn spec to search = " + dn)
        logging.debug(self._rack_mos.findall(dn))

        if "classId" in dn_elem.attrib:
            classID = dn_elem.attrib["classId"]
            for mo in self._rack_mos.findall(dn):
                for mochild in list(mo):
                    if dn_elem.attrib["inHierarchical"] == "True" or \
                       dn_elem.attrib["inHierarchical"] == "true":
                        #logging.debug(dn_elem.attrib["inDn"] + "Found")
                        if mochild.tag == classID:
                            out_xml += ElementTree.tostring(mochild)

                    else:
                        # Form a new tree with just the node and none of its children and return that
                        if mochild.tag == classID:
                            out_mo = Element(mochild.tag)
                            for attr, value in mochild.items():
                                out_mo.set(attr, value)
                            out_xml += ElementTree.tostring(out_mo)

        else:
            for mo in self._rack_mos.findall(dn):
                for mochild in list(mo):
                    if dn_elem.attrib["inHierarchical"] == "True" or \
                                    dn_elem.attrib["inHierarchical"] == "true":
                        # logging.debug(dn_elem.attrib["inDn"] + "Found")
                        out_xml += ElementTree.tostring(mochild)

                    else:
                        # Form a new tree with just the node and none of its children and return that
                        out_mo = Element(mochild.tag)
                        for attr, value in mochild.items():
                            out_mo.set(attr, value)
                        out_xml += ElementTree.tostring(out_mo)



        out_xml += ' </outConfigs> </configResolveChildren>'
        return out_xml

    def _handle_config_resolve_dn(self, dn_elem):
        logging.debug("CRDN received")
        logging.debug(dn_elem.attrib["dn"])
        logging.debug(dn_elem.attrib["inHierarchical"])

        if "cookie" not in dn_elem.attrib:
            dn_elem.attrib["cookie"] = "abcd/abcd"

        out_xml = '<configResolveDn cookie="' + dn_elem.attrib["cookie"] + '" response="yes" dn="' + dn_elem.attrib["dn"] + '"> <outConfig> '
        dn = ".//*[@dn='" + dn_elem.attrib["dn"] + "']"
        logging.debug("Dn spec to search = " + dn)
        logging.debug(self._rack_mos.findall(dn))
        for mo in self._rack_mos.findall(dn):
            if dn_elem.attrib["inHierarchical"] == "True" or \
               dn_elem.attrib["inHierarchical"] == "true":
                logging.debug(dn_elem.attrib["dn"] + "Found")
                out_xml += ElementTree.tostring(mo)
            else:
                # Form a new tree with just the node and none of its children and return that
                out_mo = Element(mo.tag)
                for attr, value in mo.items():
                    out_mo.set(attr, value)
                out_xml += ElementTree.tostring(out_mo)
        out_xml += ' </outConfig> </configResolveDn>'

        #Adding delay for configResolveDn API call for a dn based on details given in faults/fault_injector.xml file.
        if CONFIGRESOLVEDN_RESPONSE_DELAY:
            if CONFIGRESOLVEDN_RESPONSE_DELAY.has_key(dn_elem.attrib["dn"]):
                sleep(CONFIGRESOLVEDN_RESPONSE_DELAY[dn_elem.attrib["dn"]])
        return out_xml

    def _handle_config_conf_mos(self, dn_elem):
        logging.debug("CCMOs received")
        logging.debug("In Hierarchical: " + dn_elem.attrib["inHierarchical"])
        print(dn_elem)

        if "cookie" not in dn_elem.attrib:
            dn_elem.attrib["cookie"] = "abcd/abcd"

        out_xml = '<configConfMos cookie="' + dn_elem.attrib["cookie"] + '" response="yes"> <outConfigs> '

        configs = dn_elem.find('inConfigs')

        for elem in configs.iter():
            if elem.tag == "inConfigs":
                continue

            if elem.tag == "pair":
                for elemchild in list(elem):

                    dn_temp = elemchild.attrib["dn"]
                    print(dn_temp)

                    dn = ".//*[@dn='" + dn_temp + "']"
                    logging.debug("Dn spec to search = " + dn_temp)

                    for mo in self._rack_mos.findall(dn):
                        logging.debug("Config MO start: " + elemchild.tag)
                        for child_mo in mo.iter(tag=elemchild.tag):
                            logging.debug(mo.attrib)
                            for key in elemchild.attrib.keys():
                                # child_mo.attrib[key] = elem.attrib[key]
                                child_mo.set(key, elemchild.attrib[key])
                                # Send complete MO tree or just that MO based on hierarchical being true or false
                        ElementTree.Element.remove(elem, elemchild)
                        ElementTree.Element.insert(elem, 0, mo)
                out_xml += ElementTree.tostring(elem)

        out_xml += "<operationStatus>success</operationStatus></outConfigs></configConfMos>"
        return out_xml
    
    def _handle_config_conf_mo(self, dn_elem):
        logging.debug("CCMO received")
        logging.debug("DN Elem: " + ElementTree.tostring(dn_elem, method='xml'))
        logging.debug("DN: " + dn_elem.attrib["dn"])
        logging.debug("In Hierarchical: " + dn_elem.attrib["inHierarchical"])
        # logging.debug(g.imc_handle)
        # rns = dn_elem.attrib["dn"].split("/")
        # search_rn = rns[-1]
        # logging.debug("Searching for rn = " + search_rn)

        if "cookie" not in dn_elem.attrib:
            dn_elem.attrib["cookie"] = "abcd/abcd"

        out_xml = '<configConfMo cookie="' + dn_elem.attrib["cookie"] + '" response="yes" dn="' + dn_elem.attrib["dn"] + '"> <outConfig> '

        if "persist" in dn_elem.attrib:
            logging.debug("Write data to file : " + dn_elem.attrib["persist"])

        configs = dn_elem.find('inConfig')

        for elem in configs.iter():
            if elem.tag == "inConfig":
                continue

            dn_temp = elem.attrib["dn"]
            print(dn_temp)

            dn = ".//*[@dn='" + dn_temp + "']"
            logging.debug("Dn spec to search = " + dn_temp)

            for mo in self._rack_mos.findall(dn):
                logging.debug("Config MO start: " + elem.tag)
                logging.debug("Incoming Config: ")
                logging.debug(elem.attrib)
                for child_mo in mo.iter(tag=elem.tag):
                    logging.debug(mo.attrib)
                    for key in elem.attrib.keys():
                        # child_mo.attrib[key] = elem.attrib[key]
                        child_mo.set(key, elem.attrib[key])
                        # Send complete MO tree or just that MO based on hierarchical being true or false

            # this is for adding a new element
            if not self._rack_mos.findall(dn):
                # validate if the request coming is valid by comparing it with RS pre defined schema.
                logging.debug("Dn spec to search = " + dn)
                new_dn = dn_temp
                while (new_dn.rfind("/") != -1):
                    new_rn = new_dn[new_dn.rfind("/") + 1:]
                    new_dn = new_dn[0:new_dn.rfind("/")]
                    dn = ".//*[@dn='" + new_dn + "']"
                    if self._rack_mos.findall(dn):
                        break

                logging.debug(new_dn)

                for mo in self._rack_mos.findall(dn):
                    if "rn" not in elem.attrib:
                        elem.attrib["rn"] = new_rn
                    ElementTree.Element.insert(mo, 0, elem)
                    self.build_dn_for_emulator()

                if "persist" in dn_elem.attrib:
                    # self._rack_mos.write(open(self._model+"_1.xml",'w'),encoding='UTF-8',xml_declaration=True)
                    logging.debug("Write data to file : " + dn_elem.attrib["persist"])

        # For each incoming inConfig, find the subtree and replace
        # out_xml += ElementTree.tostring(mo)
        else:
            # Error out?
            pass

        dn = ".//*[@dn='" + dn_elem.attrib["dn"] + "']"
        for mo in self._rack_mos.findall(dn):
            if dn_elem.attrib["inHierarchical"] == "True" or \
                            dn_elem.attrib["inHierarchical"] == "true":
                logging.debug("Changed inHierarchical MO = " + ElementTree.tostring(mo))
                out_xml += ElementTree.tostring(mo)
            else:
                # Form a new tree with just the node and none of its children and return that
                out_mo = Element(mo.tag)
                for attr, value in mo.items():
                    out_mo.set(attr, value)
                out_xml += ElementTree.tostring(out_mo)

        out_xml += ' </outConfig> </configConfMo>'

        logging.debug("Output XML: " + out_xml)
        #Adding delay for configConfMo API call for a dn based on details given in faults/fault_injector.xml file.
        if CONFIGCONF_RESPONSE_DELAY:
            if CONFIGCONF_RESPONSE_DELAY[dn_elem.attrib["dn"]]:
                sleep(CONFIGCONF_RESPONSE_DELAY[dn_elem.attrib["dn"]])
        return out_xml

    def _handle_remove_mo(self, mo_dn):
        child_dn = ".//*[@dn='" + mo_dn + "']"
        child_mo = self._rack_mos.find(child_dn)
        if mo_dn.rfind("/") != -1:
            mo_dn = mo_dn[0:mo_dn.rfind("/")]
        parent_dn = ".//*[@dn='" + mo_dn + "']"
        output_xml = ""
        if not self._rack_mos.findall(parent_dn):
            output_xml += "Please provide correct dn for the mo"
        else:
            for mo in self._rack_mos.findall(parent_dn):
                try:
                    ElementTree.Element.remove(mo, child_mo)
                    logging.debug("Mo removed successfully.")
                    output_xml += "Mo removed sucessfully\n"
                    mo.attrib["fault"]="Removed a child mo"
                    output_xml += "Removed a child mo"
                except:
                    logging.debug("The mo was not removed, please check if the dn is correct")
        return output_xml


    def handle_request(self, request_tree):
        """ handles an incoming API request
            TODO: Validate incoming cookie and session
            """
        for elem in request_tree.iter(tag='aaaLogin'):
            return self._handle_aaaLogin(elem)

        for elem in request_tree.iter(tag='aaaLogout'):
            return self._handle_aaaLogout(elem)

        for elem in request_tree.iter(tag='configResolveClass'):
            return self._handle_config_resolve_class(elem)

        for elem in request_tree.iter(tag='configResolveDn'):
            return self._handle_config_resolve_dn(elem)

        for elem in request_tree.iter(tag='configResolveChildren'):
            return self._handle_config_resolve_children(elem)

        for elem in request_tree.iter(tag='configConfMo'):
            return self._handle_config_conf_mo(elem)

        for elem in request_tree.iter(tag='configConfMos'):
            return self._handle_config_conf_mos(elem)

        # configConfMos
    def handle_sel_login(self, sel_login):
        # TODO: Get these from an external file read to be able to inject entries
        return '''<?xml version="1.0" encoding="UTF-8"?><root><sidValue>5a35505856bbdcbac85bb885c065ac8c</sidValue><status>ok</status><authResult>0</authResult><adminUser>1</adminUser><passwordExpired>0</passwordExpired><forwardUrl>index.html</forwardUrl></root>'''

    def handle_sel_logout(self, sel_login):
#TODO: Get these from an external file read to be able to inject entries
        return ''

    def handle_sel_request(self, sel_request):
        logging.debug("SEL request" + sel_request)
        if "traceLogEntries" in sel_request:
            logging.debug("SEL traceLogEntries requested")
            return '''<root><traceLogSize>770</traceLogSize><traceLogList><traceLog><severity>Notice</severity><dateTime>2017 Feb 22 03:30:59</dateTime><source>CMC:AUDIT:2280</source><description> Login failed (ip:172.29.195.231, service:xmlapi)</description><dateTimeOrder>0</dateTimeOrder></traceLog></traceLogList><status>ok</status></root>'''

        elif "eventLogEntries" in sel_request:
            return '''
            <root><eventLogMaxEntries>3008</eventLogMaxEntries><eventLogList><eventLog><Id>3008</Id><severity>Critical</severity><dateTime>2016-06-16 08:32:28 </dateTime><dateTimeOrder>00000</dateTimeOrder><description>FRU_RAM SEL_FULLNESS: Event Log sensor for FRU_RAM, SEL Full was asserted</description></eventLog></eventLogList><status>ok</status></root>'''

