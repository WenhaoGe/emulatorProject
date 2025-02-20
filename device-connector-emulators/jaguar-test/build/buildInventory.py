from string import Template
import json
import os


def buildInventory():
    config = json.load(open('/templInv/inventory.templ'))

    if not os.path.exists('/inventory'):
        os.mkdir('/inventory')
    for k in config:
        print(k)
        out = ""
        for obj in config[k]:
            objin = open('templInv/'+ k +'.tmp')
            src = Template(objin.read())
            out += src.substitute(obj)
        outf = open('/inventory/'+k+'.xml', 'w')
        outf.write(out)
        outf.close()


buildInventory()
