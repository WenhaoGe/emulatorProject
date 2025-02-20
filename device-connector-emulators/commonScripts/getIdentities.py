import json
import argparse
import os


parser = argparse.ArgumentParser(description='Remove emulators running on a list of ports')
parser.add_argument('directory', help='directory where emulators are running')


args = parser.parse_args()

ids = []
for p in os.listdir(args.directory):
    db = json.load(open(args.directory+'/'+p+'/db/connector.db'))
    ids.append(str(db['Identity']))

out = json.dumps(ids)
print(out)
