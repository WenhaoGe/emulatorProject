from string import Template
import json
import os
import random
import string


cloud = os.getenv('EMU_CLOUD', "qaconnect.starshipcloud.com")
defaultConfig = json.load(open('/opt/db/connector.db'))
if cloud != "":
    defaultConfig['CloudDns'] = cloud

with open('/opt//db/connector.db', 'w') as dbfile:
    json.dump(defaultConfig, dbfile)

