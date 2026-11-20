FROM node:20-alpine

WORKDIR /app
COPY server.mjs ./

ENV HOST=0.0.0.0
ENV PORT=10000
EXPOSE 10000

CMD ["node", "server.mjs"]
