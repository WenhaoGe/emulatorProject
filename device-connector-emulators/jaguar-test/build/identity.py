#!/usr/bin/python
from BaseHTTPServer import BaseHTTPRequestHandler,HTTPServer
import json

PORT_NUMBER = 8000

#This class will handles any incoming request from
#the browser 
class myHandler(BaseHTTPRequestHandler):
        
        #Handler for the GET requests
        def do_GET(self):
                self.send_response(200)
                self.send_header('Content-type','text/json')
                self.end_headers()
                # Send the html message
                config = json.load(open('/opt/db/connector.db'))
                ident = {"Identity":config["Identity"], "Platform":"IMCM5"}
                self.wfile.write(json.dumps(ident))
                return

try:
        #Create a web server and define the handler to manage the
        #incoming request
        server = HTTPServer(('', PORT_NUMBER), myHandler)
        print('Started httpserver on port ' , PORT_NUMBER)
        
        #Wait forever for incoming htto requests
        server.serve_forever()

except KeyboardInterrupt:
        print('^C received, shutting down the web server')
        server.socket.close()
