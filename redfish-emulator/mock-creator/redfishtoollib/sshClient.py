''' Collect chassis inventory through ssh interface of the FI '''

import os
import paramiko
from requests.models import Response
from requests.structures import CaseInsensitiveDict
from urllib.parse import urlparse
import yaml

class SSHClient(paramiko.SSHClient):

    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self.load_system_host_keys()
        self.set_missing_host_key_policy(paramiko.WarningPolicy)

    def get(self, url, **kwargs):
        return self.request("GET", url, **kwargs)

    def request(self, method, url, **kwargs):
        ch_id = os.getenv("CHASSIS_ID", 1)
        blade_ip = os.getenv("BLADE_IP")
        url_o = urlparse(url)

        if not blade_ip:
            new_url = "http://127.5.{}.254:9000{}".format(ch_id, url_o.path)
        else:
            new_url = "https://[{}%vlan4044]{}".format(blade_ip, url_o.path)

        cmd = "curl -L -k -X {} -i \"{}\"".format(method, new_url)
        headers = kwargs.get('headers')
        if headers:
            for k,v in headers.items():
                cmd += " -H \"{}: {}\"".format(k, v)

        stdin, stdout, stderr = self.exec_command(cmd)
        lines = stdout.read().splitlines()
        r = Response()
        status_line_index = 0
        r.status_code = int(lines[status_line_index].split()[1])
        if r.status_code == 301:
            status_line_index = lines.index(b'')+1
            r.status_code = int(lines[status_line_index].split()[1])

        r.url = new_url
        json_start_index = lines.index(b'{')
        r.headers = CaseInsensitiveDict(
                yaml.load(b'\n'.join(lines[status_line_index+1:json_start_index]),
                          Loader=yaml.FullLoader))
        r._content = b'\n'.join(lines[json_start_index:])
        return r
