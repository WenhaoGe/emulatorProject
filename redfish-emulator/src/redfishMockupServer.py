# Copyright Notice:
# Copyright 2016-2019 DMTF. All rights reserved.
# License: BSD 3-Clause License. For full text see link: https://github.com/DMTF/Redfish-Mockup-Server/blob/master/LICENSE.md

# redfishMockupServer.py
# tested and developed Python 3.4

import sys
import argparse
import time
import collections
import json
import threading
import datetime
import grequests
import os
import ssl
import shutil
import logging
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import urlparse, urlunparse, parse_qs
from mock_duplicator import MockDuplicator
from rfSsdpServer import RfSSDPServer
from utils import perform_action

logger = logging.getLogger(__name__)
logger.setLevel(logging.DEBUG)
ch = logging.StreamHandler(sys.stdout)
ch.setLevel(logging.INFO)
logger.addHandler(ch)

tool_version = "1.0.9"

dont_send = ["connection", "keep-alive", "content-length", "transfer-encoding"]


def dict_merge(dct, merge_dct):
    """
    https://gist.github.com/angstwad/bf22d1822c38a92ec0a9 modified
    Recursive dict merge. Inspired by :meth:``dict.update()``, instead of
    updating only top-level keys, dict_merge recurses down into dicts nested
    to an arbitrary depth, updating keys. The ``merge_dct`` is merged into
    ``dct``.
    :param dct: dict onto which the merge is executed
    :param merge_dct: dct merged into dct
    :return: None
    """
    for k in merge_dct:
        if (k in dct and isinstance(dct[k], dict) and isinstance(merge_dct[k], collections.Mapping)):
            dict_merge(dct[k], merge_dct[k])
        else:
            dct[k] = merge_dct[k]


def clean_path(path, isShort):
    """clean_path

    :param path:
    :param isShort:
    """
    path = path.strip('/')
    path = path.split('?', 1)[0]
    path = path.split('#', 1)[0]
    if isShort:
        path = path.replace('redfish/v1', '').strip('/')
    return path


class RfMockupServer(BaseHTTPRequestHandler):
        '''
        returns index.json file for Serverthe specified URL
        '''

        def construct_path(self, path, filename):
            """construct_path

            :param path:
            :param filename:
            """
            serial = self.path.split('/')[1]
            if path.split("/")[1] != serial:
                path = "/{}{}".format(serial, path)
            apath = self.server.mockDir
            rpath = clean_path(path, self.server.shortForm)
            ret_path = '/'.join([ apath, rpath, filename ]) if filename not in ['', None] else '/'.join([ apath, rpath ])
            return ret_path

        def get_contents(self, path):
            """ get contents of the path

            :param path: path to index.json file
            """
            if os.path.isfile(path):
                with open(path) as f:
                    jsonData = json.load(f)
                    f.close()
            else:
                jsonData = None
            return jsonData is not None and jsonData != '404', jsonData

        def write_contents(self, path, contents):
            ''' Write conents to index.json file.

            :param path: path to index.json
            :param contents: contents to write to file
            '''
            with open(path, 'w') as f:
                json.dump(contents, f)

        def __generate_path_if_not_exists(self):
            if self.path == "/": return True
            serial = self.path.split('/')[1]
            if serial == "redfish" \
                    or os.path.exists(os.path.join(self.server.mockDir, serial)):
                return True

            server_pid = self.headers.get("PID", "")
            mock_base_path = os.path.join(self.server.mockDir, server_pid)
            if not server_pid or not os.path.exists(mock_base_path):
                return False

            self.__generate_path_for_serial(self.server.mockDir,
                                            server_pid,
                                            serial)
            return True

        def __generate_path_for_serial(self, base_path, server_pid, serial):
            duplicator = MockDuplicator(base_path, server_pid, serial, logger,
                                    chassis_id=self.headers.get("CHASSIS_ID"),
                                    blade_id=self.headers.get("BLADE_ID"))
            duplicator.copy()

        def try_to_sleep(self, method, path):
            """try_to_sleep

            :param method:
            :param path:
            """
            if self.server.timefromJson:
                responseTime = self.getResponseTime(method, path)
                try:
                    time.sleep(float(responseTime))
                except ValueError as e:
                    logger.info("Time is not a float value. Sleeping with default response time")
                    time.sleep(float(self.server.responseTime))
            else:
                time.sleep(float(self.server.responseTime))

        def send_header_file(self, fpath):
            """send_header_file

            :param fpath:
            """
            with open(fpath) as headers_data:
                d = json.load(headers_data)
            if isinstance(d.get("GET"), dict):
                for k, v in d["GET"].items():
                    if k.lower() not in dont_send:
                        self.send_header(k, v)

        def add_new_member(self, payload, data_received):
            members = payload.get('Members')
            n = 1
            newpath_id = data_received.get('Id', 'Member')
            newpath = '/'.join([ self.path, newpath_id ])
            while newpath in [m.get('@odata.id') for m in members]:
                n = n + 1
                newpath_id = data_received.get('Id', 'Member') + str(n)
                newpath = '/'.join([ self.path, newpath_id ])
            members.append({'@odata.id': newpath})

            payload['Members'] = members
            payload['Members@odata.count'] = len(members)
            return newpath

        def handle_eventing(self, data_received):
            sub_path = self.construct_path('/redfish/v1/EventService/Subscriptions', 'index.json')
            success, sub_payload = self.get_contents(sub_path)
            logger.info(sub_path)
            if not success:
                # Eventing not supported
                return (404)
            else:
                # Check if all of the parameters are given
                if ( ('EventType' not in data_received) or ('EventId' not in data_received) or
                        ('EventTimestamp' not in data_received) or ('Severity' not in data_received) or
                        ('Message' not in data_received) or ('MessageId' not in data_received) or
                        ('MessageArgs' not in data_received) or ('OriginOfCondition' not in data_received) ):
                    return (400)
                else:
                    # Need to reformat to make Origin Of Condition a proper link
                    origin_of_cond = data_received['OriginOfCondition']
                    data_received['OriginOfCondition'] = {}
                    data_received['OriginOfCondition']['@odata.id'] = origin_of_cond
                    event_payload = {}
                    event_payload['@odata.type'] = '#Event.v1_2_1.Event'
                    event_payload['Name'] = 'Test Event'
                    event_payload['Id'] = str(self.event_id)
                    event_payload['Events'] = []
                    event_payload['Events'].append(data_received)

                    # Go through each subscriber
                    events = []
                    for member in sub_payload.get('Members', []):
                        entry = member['@odata.id']
                        entrypath = self.construct_path(entry, 'index.json')
                        success, subscription = self.get_contents(entrypath)
                        if not success:
                            logger.info('No such resource')
                        else:
                            # Sanity check the subscription for required properties
                            if ('Destination' in subscription) and ('EventTypes' in subscription):
                                logger.info(('Target', subscription['Destination']))
                                logger.info((data_received['EventType'], subscription['EventTypes']))

                                # If the EventType in the request is one of interest to the subscriber, build an event payload
                                if data_received['EventType'] in subscription['EventTypes']:
                                    http_headers = {}
                                    http_headers['Content-Type'] = 'application/json'

                                    event_payload['Context'] = subscription.get('Context', 'Default Context')

                                    # Send the event
                                    events.append(grequests.post(subscription['Destination'], timeout=20, data=json.dumps(event_payload), headers=http_headers))
                                else:
                                    logger.info('event not in eventtypes')
                    try:
                        threading.Thread(target=grequests.map, args=(events,)).start()
                    except Exception as e:
                        logger.info('post error {}'.format( str(e)))
                    return (204)
                    self.event_id = self.event_id + 1

        def handle_telemetry(self, data_received):
            sub_path = self.construct_path('/redfish/v1/EventService/Subscriptions', 'index.json')
            success, sub_payload = self.get_contents(sub_path)
            logger.info(sub_path)
            if not success:
                # Eventing not supported
                return (404)
            else:
                # Check if all of the parameters are given
                if (('MetricReportName' in data_received) and ('MetricReportValues' in data_received)) or \
                        (('MetricReportName' in data_received) and ('GeneratedMetricReportValues' in data_received)) or \
                        (('MetricName' in data_received) and ('MetricValues' in data_received)):
                    # If the EventType in the request is one of interest to the subscriber, build an event payload
                    expected_keys = ['MetricId', 'MetricValue', 'Timestamp', 'MetricProperty', 'MetricDefinition']
                    other_keys = ['MetricProperty']
                    my_name = data_received.get('MetricName',
                                                data_received.get('MetricReportName'))
                    my_data = data_received.get('MetricValues',
                                                data_received.get('MetricReportValues',
                                                                  data_received.get('GeneratedMetricReportValues')))
                    event_payload = {}
                    value_list = []
                    # event_payload['@Redfish.Copyright'] = 'Copyright 2014-2016 Distributed Management Task Force, Inc. (DMTF). All rights reserved.'
                    event_payload['@odata.context'] = '/redfish/v1/$metadata#MetricReport.MetricReport'
                    event_payload['@odata.type'] = '#MetricReport.v1_0_0.MetricReport'
                    event_payload['@odata.id'] = '/redfish/v1/TelemetryService/MetricReports/' + my_name
                    event_payload['Id'] = my_name
                    event_payload['Name'] = my_name
                    event_payload['MetricReportDefinition'] = {
                        "@odata.id": "/redfish/v1/TelemetryService/MetricReportDefinitions/" + my_name}
                    now = datetime.datetime.now()
                    event_payload['Timestamp'] = now.strftime('%Y-%m-%dT%H:%M:%S') + ('-%02d' % (now.microsecond / 10000))

                    for tup in my_data:
                        if all(x in tup for x in expected_keys):
                            # uncomment for stricter payload check
                            # ex: if all(x in expected_keys + other_keys for x in tup):
                            value_list.append(tup)
                    event_payload['MetricValues'] = value_list
                    logger.info(event_payload)

                    # construct path "mockdir/path/to/resource/<filename>"
                    event_fpath = self.construct_path(event_payload['@odata.id'], 'index.json')
                    self.write_contents(event_fpath, event_payload)

                    report_path = '/redfish/v1/TelemetryService/MetricReports'
                    report_path = self.construct_path(report_path, 'index.json')
                    success, collection_payload = self.get_contents(report_path)

                    if not success:
                        collection_payload = {'Members': []}
                        collection_payload['@odata.context'] = '/redfish/v1/$metadata#MetricReportCollection.MetricReportCollection'
                        collection_payload['@odata.type'] = '#MetricReportCollection.v1_0_0.MetricReportCollection'
                        collection_payload['@odata.id'] = '/redfish/v1/TelemetryService/MetricReports'
                        collection_payload['Name'] = 'MetricReports'

                    if event_payload['@odata.id'] not in [member.get('@odata.id') for member in collection_payload['Members']]:
                        collection_payload['Members'].append({'@odata.id': event_payload['@odata.id']})
                    collection_payload['Members@odata.count'] = len(collection_payload['Members'])
                    self.write_contents(report_path, collection_payload)

                    # Go through each subscriber
                    events = []
                    for member in sub_payload.get('Members', []):
                        entry = member['@odata.id']
                        entrypath = self.construct_path(entry, 'index.json')
                        success, subscription = self.get_contents(entrypath)
                        if not success:
                            logger.info('No such resource')
                        else:
                            # Sanity check the subscription for required properties
                            if ('Destination' in subscription) and ('EventTypes' in subscription):
                                logger.info(('Target', subscription['Destination']))
                                http_headers = {}
                                http_headers['Content-Type'] = 'application/json'

                                # Send the event
                                events.append(grequests.post(subscription['Destination'], timeout=20, data=json.dumps(event_payload), headers=http_headers))
                            else:
                                logger.info('event not in eventtypes')
                    try:
                        threading.Thread(target=grequests.map, args=(events,)).start()
                    except Exception as e:
                        logger.info('post error {}'.format( str(e)))
                    self.event_id = self.event_id + 1
                    return (204)
                else:
                    return (400)

        server_version = "RedfishMockupHTTPD_v" + tool_version
        event_id = 1

        # Headers only request
        def do_HEAD(self):
            """do_HEAD"""
            logger.info("Headers: ")
            logger.info(self.server.headers)

            if not self.__generate_path_if_not_exists():
                logger.error("Failed to generate path")
                self.send_response(400)
                self.end_headers()
                return

            # construct path "mockdir/path/to/resource/headers.json"
            fpath = self.construct_path(self.path, 'index.json')
            fpath_xml = self.construct_path(self.path, 'index.xml')
            fpath_headers = self.construct_path(self.path, 'headers.json')
            fpath_direct = self.construct_path(self.path, '')

            # If bool headers is true and headers.json exists...
            # else, send normal headers for given resource
            if self.server.headers and (os.path.isfile(fpath_headers)):
                self.send_response(200)
                self.send_header_file(fpath_headers)
            elif (self.server.headers is False) or (os.path.isfile(fpath_headers) is False):
                if self.get_contents(fpath)[0]:
                    self.send_response(200)
                    self.send_header("Content-Type", "application/json")
                    self.send_header("OData-Version", "4.0")
                elif os.path.isfile(fpath_xml) or os.path.isfile(fpath_direct):
                    if os.path.isfile(fpath_xml):
                        file_extension = 'xml'
                    elif os.path.isfile(fpath_direct):
                        filename, file_extension = os.path.splitext(fpath_direct)
                        file_extension = file_extension.strip('.')
                    self.send_response(200)
                    self.send_header("Content-Type", "application/" + file_extension + ";odata.metadata=minimal;charset=utf-8")
                    self.send_header("OData-Version", "4.0")
                else:
                    self.send_response(404)
            else:
                self.send_response(404)
            self.end_headers()


        def do_GET(self):

            """do_GET"""
            # for GETs always dump the request headers to the console
            # there is no request data, so no need to dump that
            logger.info(("GET", self.path))
            logger.info("   GET: Headers: {}".format(self.headers))

            if not self.__generate_path_if_not_exists():
                logger.error("Failed to generate path")
                self.send_response(400)
                self.end_headers()
                return

            # construct path "mockdir/path/to/resource/<filename>"

            fpath = self.construct_path(self.path, 'index.json')
            fpath_xml = self.construct_path(self.path, 'index.xml')
            fpath_headers = self.construct_path(self.path, 'headers.json')
            fpath_direct = self.construct_path(self.path, '')

            success, payload = self.get_contents(fpath)

            scheme, netloc, path, params, query, fragment = urlparse(self.path)
            query_pieces = parse_qs(query, keep_blank_values=True)

            self.try_to_sleep('GET', self.path)

            # handle resource paths that don't exist for shortForm
            # '/' and '/redfish'
            if(self.path == '/' and self.server.shortForm):
                self.send_response(404)
                self.end_headers()

            elif(self.path in ['/redfish', '/redfish/'] and self.server.shortForm):
                self.send_response(200)
                if self.server.headers and (os.path.isfile(fpath_headers)):
                    self.send_header_file(fpath_headers)
                else:
                    self.send_header("Content-Type", "application/json")
                    self.send_header("OData-Version", "4.0")
                self.end_headers()
                self.wfile.write(json.dumps({'v1': '/redfish/v1'}, indent=4).encode())

            # if this location exists in memory or as file
            elif(success):
                # if headers exist... send information (except for chunk info)
                # end headers here (always end headers after response)
                self.send_response(200)
                if self.server.headers and (os.path.isfile(fpath_headers)):
                    self.send_header_file(fpath_headers)
                else:
                    self.send_header("Content-Type", "application/json")
                    self.send_header("OData-Version", "4.0")
                self.end_headers()

                # Strip the @Redfish.Copyright property
                output_data = payload
                output_data.pop("@Redfish.Copyright", None)

                # Query evaluate
                if output_data.get('Members') is not None:
                    my_members = output_data['Members']
                    top_count = int(query_pieces.get('$top', [str(len(my_members))])[0])
                    top_skip = int(query_pieces.get('$skip', ['0'])[0])

                    my_members = my_members[top_skip:]
                    if top_count < len(my_members):
                        my_members = my_members[:top_count]
                        query_out = {'$skip': top_skip + top_count, '$top': top_count}
                        query_string = '&'.join(['{}={}'.format(k, v) for k, v in query_out.items()])
                        output_data['Members@odata.nextLink'] = urlunparse(('', '', path, '', query_string, ''))
                    else:
                        pass

                    output_data['Members'] = my_members
                    pass

                encoded_data = json.dumps(output_data, sort_keys=True, indent=4, separators=(",", ": ")).encode()
                self.wfile.write(encoded_data)

            # if XML...
            elif(os.path.isfile(fpath_xml) or os.path.isfile(fpath_direct)):
                if os.path.isfile(fpath_xml):
                    file_extension = 'xml'
                    f = open(fpath_xml, "r")
                elif os.path.isfile(fpath_direct):
                    filename, file_extension = os.path.splitext(fpath_direct)
                    file_extension = file_extension.strip('.')
                    f = open(fpath_direct, "r")
                self.send_response(200)
                self.send_header("Content-Type", "application/" + file_extension + ";odata.metadata=minimal;charset=utf-8")
                self.send_header("OData-Version", "4.0")
                self.end_headers()
                self.wfile.write(f.read().encode())
                f.close()
            else:
                self.send_response(404)
                self.end_headers()

        def do_PATCH(self):
            logger.info(("PATCH", self.path))
            logger.info("   PATCH: Headers: {}".format(self.headers))

            if not self.__generate_path_if_not_exists():
                logger.error("Failed to generate path")
                self.send_response(400)
                self.end_headers()
                return

            self.try_to_sleep('PATCH', self.path)

            if("content-length" in self.headers):
                lenn = int(self.headers["content-length"])
                try:
                    data_received = json.loads(self.rfile.read(lenn).decode("utf-8"))
                except ValueError:
                    print ('Decoding JSON has failed, sending 400')
                    data_received = None

            if data_received:
                logger.info("   PATCH: Data: {}".format(data_received))

                # construct path "mockdir/path/to/resource/<filename>"
                fpath = self.construct_path(self.path, 'index.json')
                success, payload = self.get_contents(fpath)

                # check if resource exists, otherwise 404
                #   if it's a file, open it, if its in memory, grab it
                #   405 if Collection
                #   204 if patch success
                #   404 if payload DNE
                #   400 if no patch payload
                # end headers
                if success:
                    # If this is a collection, throw a 405
                    if payload.get('Members') is not None:
                        self.send_response(405)
                    else:
                        # After getting resource, merge the data.
                        logger.info(self.headers.get('content-type'))
                        logger.info(data_received)
                        logger.info(payload)
                        dict_merge(payload, data_received)
                        logger.info(payload)
                        self.write_contents(fpath, payload)
                        self.send_response(204)
                else:
                    self.send_response(404)
            else:
                self.send_response(400)

            self.end_headers()

        def do_PUT(self):
            logger.info(("PUT", self.path))
            logger.info("   PUT: Headers: {}".format(self.headers))

            if not self.__generate_path_if_not_exists():
                logger.error("Failed to generate path")
                self.send_response(400)
                self.end_headers()
                return

            self.try_to_sleep('PUT', self.path)

            if("content-length" in self.headers):
                lenn = int(self.headers["content-length"])
                try:
                    data_received = json.loads(self.rfile.read(lenn).decode("utf-8"))
                except ValueError:
                    print ('Decoding JSON has failed, sending 400')
                    data_received = None
                logger.info("   PUT: Data: {}".format(data_received))
            else:
                self.send_response(411)
                self.end_headers()
                return

            if data_received:
                # need to update the content in the index.json file
                # get all the key-value pairs in data-received
                # update the local index.json using the key-value pairs

                logger.info("   PUT: Data: {}".format(data_received))
                fpath = self.construct_path(self.path, 'index.json')
                # success, payload = self.get_contents(fpath)
                self.write_contents(fpath, data_received)
                self.send_response(201)
                encoded_data = json.dumps(data_received, sort_keys=True, indent=4, separators=(",", ": ")).encode()
                self.wfile.write(encoded_data)

            else:
                self.send_response(204)
            self.end_headers()

        def do_POST(self):
            logger.info(("POST", self.path))        # this is the api path
            logger.info("   POST: Headers: {}".format(self.headers))

            if not self.__generate_path_if_not_exists():
                logger.error("Failed to generate path")
                self.send_response(400)
                self.end_headers()
                return

            if("content-length" in self.headers):

                lenn = int(self.headers["content-length"])
                if lenn == 0:
                    data_received = {}
                else:
                    try:
                        data_received = json.loads(self.rfile.read(lenn).decode("utf-8"))
                    except ValueError:
                        print ('Decoding JSON has failed, sending 400')
                        data_received = None
            else:
                self.send_response(411)
                self.end_headers()
                return

            self.try_to_sleep('POST', self.path)

            if data_received is not None:
                logger.info("   POST: Data: {}".format(data_received))

                # fpath is the absolute path
                fpath = self.construct_path(self.path, 'index.json')
                success, payload = self.get_contents(fpath)

                # don't bother if this item exists, otherwise, check if its an action or a file
                # if file
                #   405 if not Collection
                #   204 if success
                #   404 if no file present
                if success:
                    if "redfish" not in fpath:
                        dependent_path = ""
                        # Find and update dependent api with posted contents
                        if "Bios2Intersight" in fpath:
                            dependent_path = fpath.replace("Bios2Intersight",
                                                        "Intersight2Bios")

                        elif "Intersight2Bios" in fpath:
                            dependent_path = fpath.replace("Intersight2Bios",
                                                        "Bios2Intersight")

                        success, payload = self.get_contents(dependent_path)
                        if success:
                            dict_merge(payload, data_received)
                            self.write_contents(dependent_path, payload)

                        self.write_contents(fpath, data_received)
                        self.send_response(200)
                        self.send_header("Content-Type", "application/json")
                        self.end_headers()
                        encoded_data = json.dumps(data_received, sort_keys=True,
                                                  indent=4, separators=(",", ": ")).encode()

                        self.wfile.write(encoded_data)
                    else:
                        # Keeping Redfish functionality
                        newpath = self.add_new_member(payload, data_received)
                        newfpath = self.construct_path(newpath, 'index.json')
                        self.write_contents(newpath, data_received)
                        self.write_contents(fpath, payload)
                        self.send_response(204)
                        self.send_header("Location", newpath)
                        self.send_header("Content-Length", "0")
                        self.end_headers()

                # Actions framework
                # index.json file does not exit in self.path
                else:

                    # SubmitTestEvent
                    if 'EventService/Actions/EventService.SubmitTestEvent' in self.path:
                        r_code = self.handle_eventing(data_received)
                        self.send_response(r_code)
                    # SubmitTestMetricReport
                    elif 'TelemetryService/Actions/TelemetryService.SubmitTestMetricReport' in self.path:
                        r_code = self.handle_telemetry(data_received)
                        self.send_response(r_code)
                    # All other actions (no data checking or response data)
                    elif '/Actions/' in self.path:
                        fpath = self.construct_path(self.path.split('/Actions/', 1)[0], 'index.json')
                        success, payload = self.get_contents(fpath)
                        if success:
                            action_found = False
                            uri_segments = self.path.split("/",2)
                            req_path = self.path
                            if uri_segments[1] != "redfish":
                                req_path = "/{}".format(uri_segments[-1])
                            try:
                                for action in payload['Actions']:
                                    if action == 'Oem':
                                        for oem_action in payload['Actions'][action]:
                                            if payload['Actions'][action][oem_action]['target'] == req_path:
                                                action_found = True
                                    else:
                                        if payload['Actions'][action]['target'] == req_path:
                                            action_found = True
                            except:
                                pass
                            if action_found:
                                perform_action(fpath, payload, action, data_received)
                                self.send_response(204)
                            else:
                                self.send_response(404)
                        else:
                            self.send_response(404)
                    # Not found
                    else:
                        # check to see if the api is not bios-related API
                        # return 404
                        paths = self.path.split('/')
                        api = paths.pop()
                        if "Bios" not in api or "BIOS" not in api:
                            self.send_response(404)
                            return

                        # create the index.json file and dump the json data to the file
                        fpath = self.construct_path(self.path, 'index.json')
                        file = open(fpath, 'w')
                        json.dump(data_received, file)
                        self.send_response(200)
                        file.close()
                        encoded_data = json.dumps(data_received, sort_keys=True, indent=4, separators=(",", ": ")).encode()
                        self.wfile.write(encoded_data)

            else:
                self.send_response(400)
            self.end_headers()

        def do_DELETE(self):
            """
            Delete a resource
            """
            logger.info("DELETE: Headers: {}".format(self.headers))

            if not self.__generate_path_if_not_exists():
                logger.error("Failed to generate path")
                self.send_response(400)
                self.end_headers()
                return

            self.try_to_sleep('DELETE', self.path)
            do_del_emu_dir = self.headers.get("DELETE_EMU_DIR")
            if do_del_emu_dir:
                logger.info("Deleting emulator directory {}".format(self.path))
                shutil.rmtree(self.construct_path(self.path, ''))
                self.send_response(204)
                self.end_headers()
                return

            fpath = self.construct_path(self.path, 'index.json')
            ppath = '/'.join(self.path.split('/')[:-1])
            parent_path = self.construct_path(ppath, 'index.json')
            success, payload = self.get_contents(fpath)

            # 404 if file doesn't exist
            # 204 if success, override payload with 404
            #   modify payload to exclude expected URI, subtract count
            # 405 if parent is not Collection
            # end headers
            if success:
                success, parentData = self.get_contents(parent_path)
                if success and parentData.get('Members') is not None:
                    self.write_contents(fpath, None)
                    parentData['Members'] = [x for x in parentData['Members'] if not x['@odata.id'] == self.path]
                    parentData['Members@odata.count'] = len(parentData['Members'])
                    self.write_contents(parent_path, parentData)
                    self.send_response(204)
                else:
                    self.send_response(405)
            else:
                self.send_response(404)

            self.end_headers()

        # Response time calculation Algorithm
        def getResponseTime(self, method, path):
            fpath = self.construct_path(path, 'time.json')
            success, item = self.get_contents(path)
            if not any(x in method for x in ("GET", "HEAD", "POST", "PATCH", "DELETE")):
                logger.info("Not a valid method")
                return (0)

            if(os.path.isfile(fpath)):
                with open(fpath) as time_data:
                    d = json.load(time_data)
                    time_str = method + "_Time"
                    if time_str in d:
                        try:
                            float(d[time_str])
                        except Exception as e:
                            logger.info(
                                "Time in the json file, not a float/int value. Reading the default time.")
                            return (self.server.responseTime)
                        return (float(d[time_str]))
            else:
                logger.info(('response time:', self.server.responseTime))
                return (self.server.responseTime)


def main():

    logger.info("Redfish Mockup Server, version {}".format(tool_version))

    parser = argparse.ArgumentParser(description='Serve a static Redfish mockup.')
    parser.add_argument('-H', '--host', '--Host', default='127.0.0.1',
                        help='hostname or IP address (default 127.0.0.1)')
    parser.add_argument('-p', '--port', '--Port', default=8000, type=int,
                        help='host port (default 8000)')
    parser.add_argument('-D', '--dir', '--Dir',
                        help='path to mockup dir (may be relative to CWD)')
    parser.add_argument('-E', '--test-etag', '--TestEtag',
                        action='store_true',
                        help='(unimplemented) etag testing')
    parser.add_argument('-X', '--headers', action='store_true',
                        help='load headers from headers.json files in mockup')
    parser.add_argument('-t', '--time', default=0,
                        help='delay in seconds added to responses (float or int)')
    parser.add_argument('-T', action='store_true',
                        help='delay response based on times in time.json files in mockup')
    parser.add_argument('-s', '--ssl', action='store_true',
                        help='place server in SSL (HTTPS) mode; requires a cert and key')
    parser.add_argument('--cert', help='the certificate for SSL')
    parser.add_argument('--key', help='the key for SSL')
    parser.add_argument('-S', '--short-form', '--shortForm', action='store_true',
                        help='apply short form to mockup (omit filepath /redfish/v1)')
    parser.add_argument('-P', '--ssdp', action='store_true',
                        help='make mockup SSDP discoverable')

    args = parser.parse_args()
    hostname = args.host
    port = args.port
    mockDirPath = args.dir
    testEtagFlag = args.test_etag
    headers = args.headers
    responseTime = args.time
    timefromJson = args.T
    sslMode = args.ssl
    sslCert = args.cert
    sslKey = args.key
    shortForm = args.short_form
    ssdpStart = args.ssdp

    logger.info('Hostname: {}'.format(hostname))
    logger.info('Port: {}'.format(port))
    logger.info("Mockup directory path specified: {}".format(mockDirPath))
    logger.info("Response time: {} seconds".format(responseTime))

    if mockDirPath is None:
        mockDirPath = "{}/mocks/".format(os.getcwd())

    # create the full path to the top directory holding the Mockup
    mockDir = os.path.realpath(mockDirPath)  # creates real full path including path for CWD to the -D<mockDir> dir path
    logger.info("Serving Mockup in absolute path: {}".format(mockDir))

    myServer = HTTPServer((hostname, port), RfMockupServer)

    if sslMode:
        logger.info("Using SSL with certfile: {}".format(sslCert))
        myServer.socket = ssl.wrap_socket(myServer.socket, certfile=sslCert, keyfile=sslKey, server_side=True)

    # save the test flag, and real path to the mockup dir for the handler to use
    myServer.mockDir = mockDir
    myServer.testEtagFlag = testEtagFlag
    myServer.headers = headers
    myServer.timefromJson = timefromJson
    myServer.shortForm = shortForm
    try:
        myServer.responseTime = float(responseTime)
    except ValueError as e:
        logger.info("Enter an integer or float value")
        sys.exit(2)
    # myServer.me="HELLO"

    mySSDP = None
    if ssdpStart:
        from gevent import monkey
        monkey.patch_all()
        # construct path "mockdir/path/to/resource/<filename>"
        path, filename, jsonData = '/redfish/v1', 'index.json', None
        apath = myServer.mockDir
        rpath = clean_path(path, myServer.shortForm)
        fpath = os.path.join(apath, rpath, filename) if filename not in ['', None] else os.path.join(apath, rpath)
        if os.path.isfile(fpath):
            with open(fpath) as f:
                jsonData = json.load(f)
                f.close()
        else:
            jsonData = None
        protocol = '{}://'.format('https' if sslMode else 'http')
        mySSDP = RfSSDPServer(jsonData, '{}{}:{}{}'.format(protocol, hostname, port, '/redfish/v1'), hostname)

    logger.info("Serving Redfish mockup on port: {}".format(port))
    try:
        if mySSDP is not None:
            t2 = threading.Thread(target=mySSDP.start)
            t2.daemon = True
            t2.start()
        logger.info('running Server...')
        myServer.serve_forever()

    except KeyboardInterrupt:
        pass

    myServer.server_close()
    logger.info("Shutting down http server")


# the below is only executed if the program is run as a script
if __name__ == "__main__":
    main()

'''
TODO:
1. add -L option to load json and dump output from python dictionary
2. add authentication support -- note that in redfish some api don't require auth
3. add https support
'''
