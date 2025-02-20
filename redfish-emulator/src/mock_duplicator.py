import json
import os
import random
import shutil
import string
from randmac import RandMac
import yaml
from utils import create_rfish_api_directories

def __rand_hex_generator(char_count):
    return ''.join(random.choices('ABCDEF' + string.digits, k=char_count))

def generate_emu_serial():
    return 'EMU' + __rand_hex_generator(8)

def generate_uuid():
    return "{}-{}-{}-{}-{}".format(__rand_hex_generator(8),
                                   __rand_hex_generator(4),
                                   __rand_hex_generator(4),
                                   __rand_hex_generator(4),
                                   __rand_hex_generator(12))

def generate_mac_addr():
    return RandMac("00:00:00:00:00:00", True).mac


class MockDuplicator(object):

    def __init__(self, path, pid, target_serial, logger,
                 chassis_id=None, blade_id=None):
        self.logger = logger
        self.path = path
        self.pid = pid
        self.chassis_id = chassis_id
        self.blade_id = blade_id
        self.target_serial = target_serial
        self.updated_macs = {}
        self.adapter_serial_map = {}
        self.generate_addtl_dirs()

    def generate_addtl_dirs(self):
        '''
        generate the directory path in PID directory
        :return: None
        '''

        base_path = os.path.join(self.path, self.pid)
        # read all data from yaml file
        with open("additional_rfish_dir.yaml") as f:
            result = yaml.load(f, Loader=yaml.FullLoader)

        create_rfish_api_directories(base_path, result)

    def copy(self):
        base_path = os.path.join(self.path, self.pid)
        new_path = os.path.join(self.path, self.target_serial)
        self.logger.info("Copying {} for {} emulator" \
                         .format(self.pid, self.target_serial))
        shutil.copytree(base_path, new_path)
        old_serial = self.__rename_serial_dir(new_path)
        self.__rename_network_adapter_dir(new_path)
        self.__update_identifiers(new_path, old_serial)

    def __set_blade_slot(self, new_path):
        self.logger.info("Updating blade slot details")
        path = os.path.join(new_path, "redfish/v1/Chassis")
        current_chass_id = None

        for item in os.listdir(path):
            if item.isdigit():
                current_chass_id = item
                break

        if not current_chass_id:
            return
        chass_folder_path = os.path.join(path, self.chassis_id)
        shutil.move(os.path.join(path, current_chass_id), chass_folder_path)
        # Set chassis id
        with open(os.path.join(chass_folder_path, 'index.json'), 'r+') as f:
            contents = f.read()
            contents.replace("/redfish/v1/Chassis/{}".format(current_chass_id),
                             "/redfish/v1/Chassis/{}".format(self.chassis_id))
            data = json.loads(contents)
            data["Id"] = self.chassis_id
            contents = json.dumps(data)
            f.seek(0)
            f.write(contents)
            f.truncate()

        # Set blade id
        with open(os.path.join(path, self.target_serial, 'index.json'), 'r+') as f:
            contents = f.read()
            data = json.loads(contents)
            data['Location']['PartLocation']['LocationOrdinalValue'] = \
                                                            int(self.blade_id)
            contents = json.dumps(data)
            f.seek(0)
            f.write(contents)
            f.truncate()
        return current_chass_id

    def __get_serial_for_chassis(self, new_path):
        path = os.path.join(new_path, "redfish/v1/Chassis")
        with open(os.path.join(path, "index.json"), 'r') as f:
            chassis = json.load(f)
        ch_path = os.path.join(new_path, chassis["Members"][0]["@odata.id"][1:])
        with open(os.path.join(ch_path, "index.json"), 'r') as f:
            ch_details = json.load(f)
        return ch_details["SerialNumber"]

    def __generate_iom_per_fi(self, new_path):
        base_path = os.path.join(new_path, "redfish/v1/Chassis")
        self.logger.info("generating IOM for FI-A")
        shutil.move(os.path.join(base_path, "CMC"),
                    os.path.join(base_path, "a/CMC"))
        self.logger.info("generating IOM for FI-B")
        shutil.copytree(os.path.join(base_path, "a"),
                        os.path.join(base_path, "b"))

    def __rename_serial_dir(self, new_path):
        path = os.path.join(new_path, "redfish/v1/Systems")
        if not os.path.exists(path):
            self.__generate_iom_per_fi(new_path)
            return self.__get_serial_for_chassis(new_path)
        old_serial = next(os.walk(path))[1][0]
        old_serial_path = os.path.join(path, old_serial)
        new_serial_path = os.path.join(path, self.target_serial)
        self.logger.info("Renaming {} with {}".format(old_serial,
                                                      self.target_serial))
        os.rename(old_serial_path, new_serial_path)
        chassis_path = os.path.join(new_path, "redfish/v1/Chassis")
        if os.path.exists(os.path.join(chassis_path, old_serial)):
            os.rename(os.path.join(chassis_path, old_serial),
                      os.path.join(chassis_path, self.target_serial))
        return old_serial

    def __update_identifiers(self, new_path, old_serial):
        self.logger.info("Updating identifiers")
        old_chassis_id = None
        if self.chassis_id and self.blade_id:
            old_chassis_id = self.__set_blade_slot(new_path)
        walk_path = os.path.join(new_path, "redfish/v1")
        for root, dirs, files in os.walk(walk_path):
            for fname in files:
                self.__update_file_contents(root, fname,
                                            old_serial,
                                            old_chassis_id)

    def __generate_serials_for_items(self, items):
        for item in items:
            item["SerialNumber"] = generate_emu_serial()
        return items

    def __rename_network_adapter_dir(self, base_path):
        self.logger.info("Generating serials for adapters")
        blade_adap_path = os.path.join(base_path, "redfish/v1/Chassis",
                                       self.target_serial, "NetworkAdapters")
        net_if_dir = os.path.join(base_path, "redfish/v1/Systems",
                                  self.target_serial, "NetworkInterfaces")

        # Check if base mock is for blade or rack
        if os.path.exists(blade_adap_path):
            net_adap_path = blade_adap_path
            net_if_dir = os.path.join(base_path, "redfish/v1/Systems",
                                      self.target_serial, "NetworkInterfaces")
            mv_source_locations = [net_if_dir]
        else:
            net_adap_path = os.path.join(base_path,
                                         "redfish/v1/Chassis/1/NetworkAdapters")
            mgr_dir = os.path.join(base_path, "redfish/v1/Managers")
            mv_source_locations = [mgr_dir, net_if_dir]

            if not os.path.exists(net_adap_path):
                return

        with open(os.path.join(net_adap_path, "index.json")) as f:
            data = json.load(f)

        for member in data["Members"]:
            adap_dir_name = os.path.basename(member["@odata.id"])
            if "_" not in adap_dir_name:
                continue
            dpath = os.path.join(net_adap_path, adap_dir_name)
            fpath = os.path.join(dpath, "index.json")
            with open(fpath) as f:
                adap = json.load(f)
            if adap["SerialNumber"] in adap_dir_name:
                serial = self.adapter_serial_map.get(adap["SerialNumber"],
                                                     generate_emu_serial())
                self.adapter_serial_map[adap["SerialNumber"]] = serial
                new_adap_dir_name = adap_dir_name.replace(adap["SerialNumber"],
                                                          serial)
                new_path = os.path.join(net_adap_path, new_adap_dir_name)
                os.rename(dpath, new_path)
                for p in mv_source_locations:
                    s = os.path.join(p, adap_dir_name)
                    if not os.path.exists(s):
                        continue
                    os.rename(s, os.path.join(p, new_adap_dir_name))

    def __update_file_contents(self, path, fname, old_serial,
                               old_chassis_id=None):
        fpath = os.path.join(path, fname)
        _, fext = os.path.splitext(fpath)
        if fext != '.json':
            return
        with open(fpath, 'r+') as f:
            contents = f.read()
            if old_serial in contents:
                contents = contents.replace(old_serial, self.target_serial)

            if self.chassis_id and old_chassis_id:
                contents = contents.replace(
                        "/redfish/v1/Chassis/{}".format(old_chassis_id),
                        "/redfish/v1/Chassis/{}".format(self.chassis_id))

            # Replace occurances of old adapter serials
            for o, n in self.adapter_serial_map.items():
                if o in contents:
                    contents = contents.replace(o, n)

            if "Serial" in contents:
                data = json.loads(contents)
                if data.get("UUID"):
                    data["UUID"] = generate_uuid()
                serial = data.get("SerialNumber")

                if "NetworkAdapter" not in data["@odata.type"].split(".") \
                        and not "#ComputerSystem" in data["@odata.id"] \
                        and serial and serial != self.target_serial:
                    data["SerialNumber"] = generate_emu_serial()
                elif "PowerSupplies" in data.keys():
                    data["PowerSupplies"] = self.__generate_serials_for_items(data["PowerSupplies"])
                contents = json.dumps(data, indent=4)
                if serial:
                    contents = contents.replace(serial, data["SerialNumber"])

            if "AssociatedNetworkAddresses" in contents \
                    or "MACAddress" in contents:
                data = self.__update_mac_addresses(contents)
                contents = json.dumps(data, indent=4)

            f.seek(0)
            f.write(contents)
            f.truncate()

    def __update_mac_addresses(self, contents):
        data = json.loads(contents)
        net_addr_list = data.get("AssociatedNetworkAddresses")
        mac = data.get("MACAddress")
        perm_mac = data.get("PermanentMACAddress")
        eth = data.get("Ethernet")
        if net_addr_list:
            old_addr = net_addr_list[0]
            new_mac = self.updated_macs.get(old_addr, generate_mac_addr())
            data["AssociatedNetworkAddresses"] = [new_mac]
        elif mac and perm_mac:
            old_addr = mac
            new_mac = self.updated_macs.get(old_addr, generate_mac_addr())
            data["MACAddress"] = new_mac
            data["PermanentMACAddress"] = new_mac
        elif eth:
            old_addr = eth["MACAddress"]
            new_mac = self.updated_macs.get(old_addr, generate_mac_addr())
            data['Ethernet']['MACAddress'] = new_mac
        else:
            # MacAddresses are empty just return data
            return data

        self.updated_macs[old_addr] = new_mac
        return data
