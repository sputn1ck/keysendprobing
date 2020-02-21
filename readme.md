# keysendprobing    

send a message to every lightning node in your node graph which supports tlv payloads and keysend payments

## howto

- go run keysendprobing.go --rpc="localhost:10009" --tls="path_to_tls.cert" --macaroon="path_to_admin.macaroon" --message="message_to_spam" --payment_amount=1 --spend_amount=10000(total sats to spend) --inflight=10(number of concurrent payments)