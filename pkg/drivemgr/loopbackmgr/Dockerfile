FROM    loopbackmgr:base

LABEL   description="Bare-metal CSI Drive Manager Service base on loopback devices"

ADD     loopbackmgr loopback-drivemgr

EXPOSE  8888

ENTRYPOINT  ["./loopback-drivemgr"]
