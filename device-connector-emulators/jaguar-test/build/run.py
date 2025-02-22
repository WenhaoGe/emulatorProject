#!/usr/bin/env python
import docker
import os
import argparse
import requests
import json
import time

# Tested with python 3.5.0

# Uses docker to run a number of connector containers
# Runs all containers detached.
# To clean up for now use: (CAUTION: will remove ALL running containers)
# docker rm -f $(docker ps -a -q)


    
parser = argparse.ArgumentParser(description='Start a number of connector containers')
parser.add_argument('persistLocation', help='directory to store connector db/logs')
parser.add_argument('numberContainers', type=int, help='number of containers to spawn')
parser.add_argument('startingPort', type=int, help='starting host port to map to')
parser.add_argument('samdb', help='path to ucsm db file')

args = parser.parse_args()
#client = docker.from_env()
client = docker.DockerClient(version='1.24')
persDir = args.persistLocation
imgNum = args.numberContainers
startingPort = args.startingPort
samdb = args.samdb

defaultConfig = {
    "CloudDns": "service-elb.andromeda.cisco.com",
    #"CloudDns": "10.106.240.110",
    "CloudCert": "\n-----BEGIN CERTIFICATE-----\nMIIDQTCCAimgAwIBAgITBmyfz5m/jAo54vB4ikPmljZbyjANBgkqhkiG9w0BAQsF\nADA5MQswCQYDVQQGEwJVUzEPMA0GA1UEChMGQW1hem9uMRkwFwYDVQQDExBBbWF6\nb24gUm9vdCBDQSAxMB4XDTE1MDUyNjAwMDAwMFoXDTM4MDExNzAwMDAwMFowOTEL\nMAkGA1UEBhMCVVMxDzANBgNVBAoTBkFtYXpvbjEZMBcGA1UEAxMQQW1hem9uIFJv\nb3QgQ0EgMTCCASIwDQYJKoZIhvcNAQEBBQADggEPADCCAQoCggEBALJ4gHHKeNXj\nca9HgFB0fW7Y14h29Jlo91ghYPl0hAEvrAIthtOgQ3pOsqTQNroBvo3bSMgHFzZM\n9O6II8c+6zf1tRn4SWiw3te5djgdYZ6k/oI2peVKVuRF4fn9tBb6dNqcmzU5L/qw\nIFAGbHrQgLKm+a/sRxmPUDgH3KKHOVj4utWp+UhnMJbulHheb4mjUcAwhmahRWa6\nVOujw5H5SNz/0egwLX0tdHA114gk957EWW67c4cX8jJGKLhD+rcdqsq08p8kDi1L\n93FcXmn/6pUCyziKrlA4b9v7LWIbxcceVOF34GfID5yHI9Y/QCB/IIDEgEw+OyQm\njgSubJrIqg0CAwEAAaNCMEAwDwYDVR0TAQH/BAUwAwEB/zAOBgNVHQ8BAf8EBAMC\nAYYwHQYDVR0OBBYEFIQYzIU07LwMlJQuCFmcx7IQTgoIMA0GCSqGSIb3DQEBCwUA\nA4IBAQCY8jdaQZChGsV2USggNiMOruYou6r4lK5IpDB/G/wkjUu0yKGX9rbxenDI\nU5PMCCjjmCXPI6T53iHTfIUJrU6adTrCC2qJeHZERxhlbI1Bjjt/msv0tadQ1wUs\nN+gDS63pYaACbvXy8MWy7Vu33PqUXHeeE6V/Uq2V8viTO96LXFvKWlJbYK8U90vv\no/ufQJVtMVT8QtPHRh8jrdkPSHCa2XV4cdFyQzR1bldZwgJcJmApzyMZFo6IQ6XU\n5MsI+yMRQ+hDKXJioaldXgjUkK642M4UwtBV8ob2xJNDd2ZhwLnoQdeXeGADbkpy\nrqXRfboQnoZsG4q5WTP468SQvvG5\n-----END CERTIFICATE-----\n-----BEGIN CERTIFICATE-----\nMIIE8TCCA9mgAwIBAgIKGn4kGQAAAAAADDANBgkqhkiG9w0BAQsFADAxMRYwFAYD\nVQQKEw1DaXNjbyBTeXN0ZW1zMRcwFQYDVQQDEw50cmNhLTIwNDgtc2hhMjAeFw0x\nNDExMTYyMzQwNDdaFw00NDA3MDkxNjU2MzZaMDYxCzAJBgNVBAYTAlVTMQ4wDAYD\nVQQKEwVDaXNjbzEXMBUGA1UEAxMOdHNjYS0yMDQ4LXNoYTIwggEiMA0GCSqGSIb3\nDQEBAQUAA4IBDwAwggEKAoIBAQCkSMir9qtlp9qsBSKXMY4SKB+NUozwheNNYJqa\nRwcxThBYaeXx0XEt1XtiXaVA7q/a463/ftTbWqvXf3SbcAodTN7iVFvzX7APaVWS\nHVRZqFdo0M9sRkbhEZX7+ELAs5BGOmu6gzoSKJ3Ka6roeSN+KnQRaBKSGtOQOe+k\n7mzicTAfyndi5X0JfGfXkr0Xdz9CFQSkjMCwA7NCrx4SJJhA9127jw2wU7FpT9pw\n09oEl7V+ed/G6QorDGKaJ41r1UJ3w/tkOYEM/lBMtoPwdePVepgknvkvcMqd/duo\nUY7tVF4ZEqGEwQ/EAMUmjorwXY5JG2usnJCq5K5JrvWZv8GLAgMBAAGjggIEMIIC\nADAQBgkrBgEEAYI3FQEEAwIBADAdBgNVHQ4EFgQUcRRfe4KgXmTE0HLlV/gkRTz/\n9zkwGQYJKwYBBAGCNxQCBAweCgBTAHUAYgBDAEEwCwYDVR0PBAQDAgGGMBIGA1Ud\nEwEB/wQIMAYBAf8CAQAwHwYDVR0jBBgwFoAUsYp8CcDZxs6WrOyjBx4vqVhXmmEw\nRwYDVR0fBEAwPjA8oDqgOIY2aHR0cDovL3RyY2EtMjA0OC1zaGEyLmNpc2NvLmNv\nbS9jcmwvdHJjYS0yMDQ4LXNoYTIuY3JsMFMGCCsGAQUFBwEBBEcwRTBDBggrBgEF\nBQcwAoY3aHR0cDovL3RyY2EtMjA0OC1zaGEyLmNpc2NvLmNvbS9jZXJ0L3RyY2Et\nMjA0OC1zaGEyLmNydDBcBgNVHSAEVTBTMFEGCisGAQQBCRUBAQAwQzBBBggrBgEF\nBQcCARY1aHR0cDovL3d3dy5jaXNjby5jb20vc2VjdXJpdHkvcGtpL3BvbGljaWVz\nL2luZGV4Lmh0bWwwdAYDVR0lBG0wawYIKwYBBQUHAwEGCCsGAQUFBwMCBggrBgEF\nBQcDBQYIKwYBBQUHAwYGCCsGAQUFBwMHBggrBgEFBQcDCQYKKwYBBAGCNwoDAQYK\nKwYBBAGCNwoDCQYKKwYBBAGCNxQCAQYJKwYBBAGCNxUGMA0GCSqGSIb3DQEBCwUA\nA4IBAQDC4Z9ZgXas5m3Pi5C7S9hDs0g1+EIIJ+UQd93suRD2fbFt0BA7ynyR3FjK\nRQfJuWT5fPGrbasRWT/LAZERmmqVaumdaRWDOFpUhmjs3YCnQtz/H6seleZPMth6\nXl1wu/wb+ibXqmdWZrhiuN0EURvSyFd1AhMqBCwlCk4EPOOHewxWq+W0FR4XmxVO\natI4jmPojx4s/+VlNcgCf8dsUbDew4NwupkllwKUywCZ0vvlyBbwKga3PNg5AdnP\n9ZiiNbb2pVn+AvzAmNgCi05cE46MInqIaKPROIFRvMkqfuSnpfqTtTlmSrjfwPoS\nVeNaa5OROShNC8PwfLgZDSWI2nY/\n-----END CERTIFICATE-----\n-----BEGIN CERTIFICATE-----\nMIIDQzCCAiugAwIBAgIQX/h7KCtU3I1CoxW1aMmt/zANBgkqhkiG9w0BAQUFADA1\nMRYwFAYDVQQKEw1DaXNjbyBTeXN0ZW1zMRswGQYDVQQDExJDaXNjbyBSb290IENB\nIDIwNDgwHhcNMDQwNTE0MjAxNzEyWhcNMjkwNTE0MjAyNTQyWjA1MRYwFAYDVQQK\nEw1DaXNjbyBTeXN0ZW1zMRswGQYDVQQDExJDaXNjbyBSb290IENBIDIwNDgwggEg\nMA0GCSqGSIb3DQEBAQUAA4IBDQAwggEIAoIBAQCwmrmrp68Kd6ficba0ZmKUeIhH\nxmJVhEAyv8CrLqUccda8bnuoqrpu0hWISEWdovyD0My5jOAmaHBKeN8hF570YQXJ\nFcjPFto1YYmUQ6iEqDGYeJu5Tm8sUxJszR2tKyS7McQr/4NEb7Y9JHcJ6r8qqB9q\nVvYgDxFUl4F1pyXOWWqCZe+36ufijXWLbvLdT6ZeYpzPEApk0E5tzivMW/VgpSdH\njWn0f84bcN5wGyDWbs2mAag8EtKpP6BrXruOIIt6keO1aO6g58QBdKhTCytKmg9l\nEg6CTY5j/e/rmxrbU6YTYK/CfdfHbBcl1HP7R2RQgYCUTOG/rksc35LtLgXfAgED\no1EwTzALBgNVHQ8EBAMCAYYwDwYDVR0TAQH/BAUwAwEB/zAdBgNVHQ4EFgQUJ/PI\nFR5umgIJFq0roIlgX9p7L6owEAYJKwYBBAGCNxUBBAMCAQAwDQYJKoZIhvcNAQEF\nBQADggEBAJ2dhISjQal8dwy3U8pORFBi71R803UXHOjgxkhLtv5MOhmBVrBW7hmW\nYqpao2TB9k5UM8Z3/sUcuuVdJcr18JOagxEu5sv4dEX+5wW4q+ffy0vhN4TauYuX\ncB7w4ovXsNgOnbFp1iqRe6lJT37mjpXYgyc81WhJDtSd9i7rp77rMKSsH0T8lasz\nBvt9YAretIpjsJyp8qS5UwGH0GikJ3+r/+n6yUA4iGe0OcaEb1fJU9u6ju7AQ7L4\nCYNu/2bPPu8Xs1gYJQk0XuPL1hS27PKSb3TkL4Eq1ZKR4OCXPDJoBYVL0fdX4lId\nkxpUnwVwwEpxYB5DC2Ae/qPOgRnhCzU=\n-----END CERTIFICATE-----\n",
    "Identity": "",
    "AccessKeyId": "xr0NUk93Dl7rV3gwCMluumIVb3IKMxqrJvmI1esq-4eNqYrt",
    "AccessKey": "Wbnb_FU62bINCw13Ki5NpB6-Ik6K0cHWvfDnQCyFl4I9am9L",
    "CloudEnabled": True,
    "HttpProxy": {
        "ProxyType": "Disabled",
        "ProxyHost": "",
        "ProxyPort": 0,
        "ProxyUsername": "",
        "ProxyPassword": "JpALjKSEqajWEwzla6V0bQ=="
    },
    "AccountOwnershipState": "Not Claimed",
    "AccountOwnershipId": "",
    "AccoutOwnershipUser": "",
    "AccountOwnershipTime": "0001-01-01T00:00:00Z"
}

imcdcTest = 'imcdc'

for i in range(0, imgNum):
    if not os.path.exists(persDir + str(i)):
        os.makedirs(persDir + str(i))
        os.makedirs(persDir + str(i) + '/db')
        os.makedirs(persDir + str(i) + '/logs')
        os.makedirs(persDir + str(i) + '/faults')
        with open(persDir + str(i) + '/db/connector.db', 'w') as dbfile:
            json.dump(defaultConfig, dbfile)
        with open('faults/fault_injector.xml') as f:
            lines = f.readlines()
            with open(persDir + str(i) + '/faults/fault_injector.xml', 'w') as f1:
                f1.writelines(lines)
    open(persDir + str(i) + '/logs/emulator.log', 'w')
    open(persDir + str(i) + '/logs/device_connector.log', 'w')
    # open(persDir + str(i) + '/faults/fault_injector.xml', 'w')

    ip = str(i) + "." + str(i) + "." + str(i) + "." + str(i)
    hostname = "devimc" + str(i)
    envs = ["ENV_IP=" + ip, "ENV_HOSTNAME=" + hostname, "GORACE=/logs/raceErrs"]
    ports = {80: startingPort + i}
    vols = {persDir + str(i) + '/db': '/db', persDir + str(i) + '/logs': '/logs', persDir + str(i) + '/faults': '/faults'}
    client.containers.run(imcdcTest, volumes=vols, ports=ports, environment=envs, detach=True)
