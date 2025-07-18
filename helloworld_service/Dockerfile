FROM oven/bun:alpine AS hello-service

RUN apk add --no-cache curl

WORKDIR /app

COPY package.json ./
RUN bun install --frozen-lockfile

COPY . .

EXPOSE 8080

CMD ["bun", "run", "start"]
