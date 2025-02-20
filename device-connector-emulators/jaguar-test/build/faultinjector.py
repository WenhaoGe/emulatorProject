import threading
import logging
from watchdog.observers import Observer
from watchdog.events import PatternMatchingEventHandler
from xml.etree import ElementTree

import emulator

LOG_FILENAME = './logs/emulator.log'
logging.basicConfig(format='%(asctime)s:%(message)s' , datefmt='%m/%d/%Y %I:%M:%S %p', filename=LOG_FILENAME, level=logging.DEBUG)
'''We have different types of faults like threshold/value based, delay, missing dn fault, etc. 
threshold fault : If an attribute of a dn is not matching expected value. For example, temperature of cpu is more than a value.  
delay : API responses for any dn are delayed based on values provided by user in fauly_injector.xml file. 
missing dn fault: This is type fault where we remove a dn, like we remove a disk. We flag an error if this fault is injected.   
'''
class MyHandler(PatternMatchingEventHandler):
    patterns = ["*.xml"]

    def __init__(self, emulator=None):
        super(MyHandler, self).__init__()
        self.emulator = emulator

    def process(self, event):
        with open(event.src_path, 'r') as xml_source:
            xml_string = xml_source.read()
            if xml_string.strip(' '):
                print(event.src_path)
                self._faults = ElementTree.ElementTree(ElementTree.fromstring(xml_string))
                emulator.CONFIGCONF_RESPONSE_DELAY={}
                emulator.CONFIGRESOLVEDN_RESPONSE_DELAY={}
                emulator.CONFIGRESOLVECLASS_RESPONSE_DELAY={}
                for fault in self._faults.iter():
                    if fault.tag == "fault":
                        if fault.attrib["type"] == "Threshold":  #threshold fault
                            for f in fault:
                                f.attrib["inHierarchical"] = "true"
                                f.attrib["cookie"] = "empty_cookie"
                                fault_attrib = f.attrib["attr_name"]
                                fault_attrib_max_value = f.attrib["max_value"]
                                if f.tag == "configConfMo":          #We modify the desired attribute with configConfMo API and based on response we print the fault to the logs.
                                    if self.emulator:
                                        output_xml = self.emulator._handle_config_conf_mo(f)
                                        if output_xml:
                                            print(output_xml)
                                            res_mo = ElementTree.ElementTree(ElementTree.fromstring(output_xml))
                                            if res_mo:
                                                for res in res_mo.iter():
                                                    if res.tag == "configConfMo":
                                                        continue
                                                    if res.tag == "outConfig":
                                                        continue
                                                    if res.attrib[fault_attrib] > fault_attrib_max_value:
                                                        logging.debug("There is a fault ")

                                # if f.tag == "fault_attr":
                                #     print (f.attrib["attr_name"])
                                #     output_xml = ""
                                #     if self.emulator:
                                #         output_xml = self.emulator._handle_config_resolve_dn(f)
                                #
                                #     if output_xml:
                                #         print (output_xml)
                                #     logging.debug(f.attrib["attr_name"])

                        elif fault.attrib["type"] == "Missing-dn": #missing  dn fault
                            #remove the dn and update fault attribute in the parent.
                            logging.debug("remove the dn and update the fault attribute in the parent")
                            mo_dn = fault.attrib["dn"]
                            if self.emulator:
                                output_xml = self.emulator._handle_remove_mo(mo_dn)

                            if output_xml:
                                logging.debug(output_xml)


                            logging.debug(mo_dn)


                    elif fault.tag == "cimc-behaviour":
                        if fault.attrib["type"] == "Delay":  # delay fault
                            apitype = fault.attrib["apitype"]
                            if apitype:  # based on API type we populate the dictionary with dn:delay/classId:delay key value pairs
                                if apitype == "ConfigResolveDn":
                                    logging.debug("ConfigResolveDn Delay response")
                                    emulator.CONFIGRESOLVEDN_RESPONSE_DELAY[fault.attrib["dn"]] = int(fault.attrib["delay_time"])
                                elif apitype == "ConfigConfMo":
                                    logging.debug("ConfigConfMo Delay response")
                                    emulator.CONFIGCONF_RESPONSE_DELAY[fault.attrib["dn"]] = int(fault.attrib["delay_time"])
                                elif apitype == "ConfigResolveClass":
                                    logging.debug("ConfigResolveClass Delay response")
                                    emulator.CONFIGRESOLVECLASS_RESPONSE_DELAY[fault.attrib["classId"]] = int(
                                        fault.attrib["delay_time"])
                            else:
                                logging.debug("The API type is wrong, please provide correct API type")

            logging.debug(xml_string)

    def on_modified(self, event):
        self.process(event)

    def on_created(self, event):
        self.process(event)

'''Watchdog is a python library for file monitoring 
The Observer object is used to created a backgroud thread that constantly listens to file operations 
done in a particular folder and responds accordingly. 

In our case we monitoring the faults folder for xml file types.
Any modification in the file will inject a fault, if the fault is in proper format. 
'''


class FaultInjector(object):
    def __init__(self, path, emulator):
        self._path = path
        self._emulator = emulator

    def start(self):
        event_handler = MyHandler(self._emulator)
        observer = Observer()
        observer.schedule(event_handler, self._path)
        try:
            thread = threading.Thread(target=observer.start(), args=None)
            thread.daemon = True
            thread.start()
        except:
            logging.debug("There was problem starting the fault injector observer")
