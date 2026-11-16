# Send association and DHCP events to the sensor in real time.
# 2001:db8:0:10::5 is the host running mikrotik-presence-sensor.

/system logging action add name=presence target=remote remote=2001:db8:0:10::5 remote-port=5514 \
    bsd-syslog=yes syslog-facility=daemon
# RouterOS versions with TCP remote logging can use it instead of UDP
# (the sensor listens on both), e.g. add: remote-protocol=tcp

# Wi-Fi (both the wifi and the legacy wireless package) and CAPsMAN
/system logging add topics=wireless,info action=presence
/system logging add topics=caps,info action=presence
# DHCP server (only on the router)
/system logging add topics=dhcp,info action=presence
