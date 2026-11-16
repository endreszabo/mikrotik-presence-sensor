# HTTPS for the REST API with a private CA. Run the CA part once (on one
# device), then issue one certificate per device. Adjust names/addresses.

# --- once: create the CA ---
/certificate add name=presence-ca common-name=presence-ca key-usage=key-cert-sign,crl-sign days-valid=3650
/certificate sign presence-ca
# Export the CA certificate and copy presence-ca.crt to the sensor host
# (Files menu or scp), then point rest.ca_file at it.
/certificate export-certificate presence-ca export-passphrase=""

# --- per device: server certificate and service ---
/certificate add name=https-ap-living-room common-name=ap-living-room.home.arpa \
    subject-alt-name=DNS:ap-living-room.home.arpa,IP:2001:db8:0:10::2 \
    key-usage=digital-signature,key-encipherment,tls-server days-valid=825
/certificate sign https-ap-living-room ca=presence-ca
/ip service set www-ssl certificate=https-ap-living-room tls-version=only-1.2 disabled=no \
    address=2001:db8:0:10::5/128

# Alternatively, on devices reachable from the Internet, Let's Encrypt:
# /certificate enable-ssl-certificate dns-name=ap.example.com
