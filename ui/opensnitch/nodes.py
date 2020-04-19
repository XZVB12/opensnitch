from queue import Queue
from datetime import datetime

class Nodes():
    __instance = None
    LOG_TAG = "[Nodes]: "

    @staticmethod
    def instance():
        if Nodes.__instance == None:
            Nodes.__instance = Nodes()
        return Nodes.__instance

    def __init__(self):
        self._nodes = {}

    def count(self):
        return len(self._nodes)

    def add(self, context, client_config=None):
        try:
            proto, addr = self.get_addr(context.peer())
            addr = "%s:%s" % (proto, addr)
            if addr not in self._nodes:
                self._nodes[addr] = {
                        'notifications': Queue(),
                        'online':        True,
                        'last_seen':     datetime.now()
                        }
                self.add_data(addr, client_config)
                return self._nodes[addr]

            self._nodes[addr]['last_seen'] = datetime.now()
            self.add_data(addr, client_config)

            return self._nodes[addr]

        except Exception as e:
            print(self.LOG_TAG + " exception adding/updating node: ", e, addr, client_config)

        return None

    def add_data(self, addr, client_config):
        if client_config != None:
            self._nodes[addr]['data'] = client_config

    def delete_all(self):
        self.send_notifications(None)
        self._nodes = {}

    def delete(self, peer):
        proto, addr = self.get_addr(peer)
        addr = "%s:%s" % (proto, addr)
        # Force the node to get one new item from queue,
        # in order to loop and exit.
        self.send_notification(addr, None)
        if addr in self._nodes:
            del self._nodes[addr]

    def get(self):
        return self._nodes

    def get_addr(self, peer):
        peer = peer.split(":")
        return peer[0], peer[1]

    def get_notifications(self):
        notlist = []
        try:
            for c in self._nodes:
                if self._nodes[c]['online'] == False:
                    continue
                if self._nodes[c]['notifications'].empty():
                    continue
                notif = self._nodes[c]['notifications'].get(False)
                if notif != None:
                    self._nodes[c]['notifications'].task_done()
                    notlist.append(notif)
        except Exception as e:
            print(self.LOG_TAG + " exception get_notifications(): ", e)

        return notlist

    def save_node_config(self, addr, config):
        try:
            self._nodes[addr]['data'].config = config
        except Exception as e:
            print(self.LOG_TAG + " exception saving node config: ", e, addr, config)

    def save_nodes_config(self, config):
        try:
            for c in self._nodes:
                self._nodes[c]['data'].config = config
        except Exception as e:
            print(self.LOG_TAG + " exception saving nodes config: ", e, config)

    def send_notification(self, addr, notification):
        try:
            self._nodes[addr]['notifications'].put(notification)
        except Exception as e:
            print(self.LOG_TAG + " exception sending notification: ", e, addr, notification)

    def send_notifications(self, notification):
        """
        Enqueues a notification to the clients queue.
        It'll be retrieved and delivered by get_notifications
        """
        try:
            for c in self._nodes:
                self._nodes[c]['notifications'].put(notification)
        except Exception as e:
            print(self.LOG_TAG + " exception sending notifications: ", e, notification)
