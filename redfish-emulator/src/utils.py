import json
import os
import threading
import yaml

def create_rfish_api_directories(path, content):
    '''

    :param path: is the directory path
    :param content: is the directory object, initially it is the whole content in the yaml file
    :return: None
    '''
    for k, v in content.items():
        new_path = os.path.join(path, k)
        os.makedirs(new_path, exist_ok=True)
        if isinstance(v, dict):
            create_rfish_api_directories(new_path,v)
        else:
            file_path = os.path.join(path, k)
            create_indexjs(file_path, v)

def create_indexjs(filepath, content):
    '''

    :param filepath: file path of index.json file
    :param content: the content that needs to be written into index.json file
    :return: None
    '''
    filepath = os.path.join(filepath, "index.json")
    if os.path.isfile(filepath):
        # if the index.json file already existed
        return
    elif content:
        # if the index.json file does not exist, create one and write content to it
        with open(filepath, 'w') as f:
            f.write(content)

def perform_action(fpath, contents, action_name, data_recieved):
    with open("actions.yaml") as f:
        actions = yaml.load(f, Loader=yaml.FullLoader)

    action = actions.get(action_name)
    if not action: return

    for k, v in data_recieved.items():
        for state in action.get(k, {}).get(v, []):
            delay = state.pop("delay")
            thread = threading.Timer(delay, __set_state,
                                     args=(fpath, contents, state))
            thread.start()

def __set_state(fpath, contents, state):
    contents.update(state)
    with open(fpath, 'w') as f:
        json.dump(contents, f)
