# Build stage
FROM node:20.14.0-alpine3.20 AS builder

WORKDIR /home/node/app
COPY . .

ENV PUPPETEER_SKIP_CHROMIUM_DOWNLOAD 1
ENV PUPPETEER_EXECUTABLE_PATH /usr/bin/chromium-browser

RUN npm install --omit=dev
RUN npm run build

# App stage
FROM zenika/alpine-chrome:with-node AS app

USER root

RUN apk add --no-cache bash shadow socat \
    && gpasswd -a chrome dialout
RUN mkdir -p /usr/src/app/node_modules \
    && chown -R chrome:chrome /usr/src/app
WORKDIR /usr/src/app

COPY --from=builder --chown=chrome:chrome /home/node/app/dist .
COPY --from=builder --chown=chrome:chrome /home/node/app/node_modules ./node_modules

USER chrome

ENV PUPPETEER_SKIP_CHROMIUM_DOWNLOAD 1
ENV PUPPETEER_EXECUTABLE_PATH /usr/bin/chromium-browser
ENV NODE_ENV=production

ENTRYPOINT ["node", "index.js"]
CMD []
