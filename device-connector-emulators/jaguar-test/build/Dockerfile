FROM resin/armv7hf-debian-qemu
RUN [ "cross-build-start" ]
RUN echo Hello from ARM container

RUN apt-get update && apt-get install -y nginx python-pip

RUN pip install flask imcsdk watchdog
RUN apt-get install squashfs-tools

RUN apt-get install -y curl
ADD start.sh /start.sh

ADD emulator.py /emulator.py
ADD faultinjector.py /faultinjector.py
ADD route.py /route.py
ADD nginx.conf /nginx.conf
ADD rack.cfg /rack.cfg
ADD start.sh /start.sh
ADD UCSC-C220-M4L.xml /UCSC-C220-M4L.xml
ADD UCSC-C220-M5L.xml /UCSC-C220-M5L.xml
ADD HXAF220C-M5SX.xml /HXAF220C-M5SX.xml

ADD nginx.conf /nginx.conf

RUN touch /.device_connector_emulator

ADD install-connector-early.sh /nuova/bin/install-connector-early.sh
RUN chmod +x /nuova/bin/install-connector-early.sh

ADD identity.py /identity.py
ADD setupdb.py /setupdb.py
ADD connector.db /opt/db/connector.db


RUN mkdir -p /opt/db
RUN mkdir -p /var/log/dc
RUN mkdir -p /var/nuova/sync
COPY ucsc-connector.bin /mnt/scratchpad/cloudimgfs.img
RUN /nuova/bin/install-connector-early.sh /mnt/scratchpad/cloudimgfs.img

ADD ucs-mgmt-cloud-connector /etc/init.d/ucs-mgmt-cloud-connector

ENV QEMU_EXECVE 1

RUN [ "cross-build-end" ]

ENTRYPOINT [ "qemu-arm-static" ]
CMD ["/bin/bash", "/start.sh"]
