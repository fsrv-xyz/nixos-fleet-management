CA_DIR := ca

.PHONY: generate-certificates
generate-certificates:
	mkdir -p $(CA_DIR)
	openssl req -x509 -newkey ed25519 -noenc -keyout $(CA_DIR)/ca.key -out $(CA_DIR)/ca.crt -days 3650 \
		  -subj "/CN=Local CA" \
		  -addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,cRLSign"

	openssl req -x509 -newkey ed25519 -noenc -keyout $(CA_DIR)/server.key -out $(CA_DIR)/server.crt -days 825 \
		  -CA $(CA_DIR)/ca.crt -CAkey $(CA_DIR)/ca.key -subj "/CN=localhost" \
		  -addext "basicConstraints=CA:FALSE" -addext "extendedKeyUsage=serverAuth" \
		  -addext "subjectAltName=DNS:localhost,IP:127.0.0.1,IP:::1"

