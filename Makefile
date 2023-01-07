.PHONY: all build run datastore deploy

gcloud:=gcloud --project=$${PROJECT}

all: build

build: dm-retweeter

tweet-saver: $(wildcard *.go go.*)
	go build .

run:
	$$($(gcloud) beta emulators datastore env-init --data-dir=datastore-emulator); \
	source .env; \
	export GOOGLE_CLOUD_PROJECT="$${PROJECT}"; \
	go run .

datastore:
	$(gcloud) beta emulators datastore start --data-dir=datastore-emulator

deploy:
	$(gcloud) app deploy --quiet

setup_credentials:
	source .env; \
	$(gcloud) beta runtime-config configs create prod; \
	$(gcloud) beta runtime-config configs variables set --config-name=prod --is-text twitter/api_key "$${TWITTER_API_KEY}"; \
	$(gcloud) beta runtime-config configs variables set --config-name=prod --is-text twitter/api_key_secret "$${TWITTER_API_KEY_SECRET}"; \
	$(gcloud) beta runtime-config configs variables set --config-name=prod --is-text twitter/bearer_token "$${TWITTER_BEARER_TOKEN}"; \
	$(gcloud) beta runtime-config configs variables set --config-name=prod --is-text twitter/client_id "$${TWITTER_CLIENT_ID}"; \
	$(gcloud) beta runtime-config configs variables set --config-name=prod --is-text twitter/client_secret "$${TWITTER_CLIENT_SECRET}"; \
	$(gcloud) beta runtime-config configs variables set --config-name=prod --is-text twitter/bot_user_id "$${BOT_USER_ID}"; \
	$(gcloud) beta runtime-config configs variables set --config-name=prod --is-text hostname "$${HOSTNAME}"; \
