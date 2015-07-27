import gevent
from gevent import monkey
monkey.patch_all()
import sys
import requests
import datetime
import json
import random

SHOW_ID = "143796412990036"
SERVER_URL = "http://stage.bolo.me:34567/api/live_show"


def send_heartbeat(client_no, show_id, cookie_str, sleep_time):
    gevent.sleep(random.randrange(1, 10))
    while 1:
        data = {"show_id": show_id}
        headers = {
            "Cookie": "h5_user=" + cookie_str,
            "Content-Type": "application/x-www-form-urlencoded"
        }
        print "client %d start, with cookie: %s" % (client_no, cookie_str)
        resp = requests.post(SERVER_URL, json=data, headers=headers)
        print "got response:", resp.status_code
        gevent.sleep(sleep_time)


def start(num_of_clients):
    task_pool = []
    for i in range(num_of_clients):
        cookie_str = datetime.datetime.now().strftime("%H%M%S%f")
        task = gevent.spawn(send_heartbeat, i, SHOW_ID, cookie_str, 3)
        task_pool.append(task)
    gevent.wait(task_pool)


if __name__ == "__main__":
    start(int(sys.argv[1]))
