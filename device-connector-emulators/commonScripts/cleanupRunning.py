import argparse
import os
import shutil
import subprocess
import sys


parser = argparse.ArgumentParser(description='Remove emulators running on a list of ports')
parser.add_argument('directory', help='directory where emulators are running')
parser.add_argument('--retain', nargs='?', const="", help='retain the connecotr database')


args = parser.parse_args()

if not os.path.exists(args.directory):
    print("nothing to do")
    sys.exit(0)
for p in os.listdir(args.directory):
    print(p)
    try:
        proc = subprocess.Popen(['docker', 'rm', '-f', 'dcemul'+str(p)])
        proc.communicate()
    except Exception as e:
        print("failed to kill container", e)
    if args.retain is None:
        shutil.rmtree(args.directory+'/'+str(p))
