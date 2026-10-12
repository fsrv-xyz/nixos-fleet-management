CA_DIR := ca

.PHONY: generate-certificates
generate-certificates:
	mkdir -p $(CA_DIR)
	openssl req -x509 -newkey ed25519 -noenc -keyout $(CA_DIR)/ca.key -out $(CA_DIR)/ca.crt -days 3650 \
		  -subj "/CN=Local CA" \
		  -addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,cRLSign"
