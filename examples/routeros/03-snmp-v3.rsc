# SNMPv3 read-only access (the community name is the v3 user name).
# Older RouterOS versions only offer MD5/SHA1 here; then use SHA1 and set
# snmp.auth_protocol: SHA in the sensor configuration.
/snmp community add name=presence addresses=2001:db8:0:10::5/128 security=private \
    authentication-protocol=SHA256 authentication-password="CHANGE-ME-auth" \
    encryption-protocol=AES encryption-password="CHANGE-ME-priv" \
    read-access=yes write-access=no
/snmp set enabled=yes

# SNMPv2c alternative (cleartext, avoid on untrusted networks):
# /snmp community add name=CHANGE-ME addresses=2001:db8:0:10::5/128 read-access=yes write-access=no
