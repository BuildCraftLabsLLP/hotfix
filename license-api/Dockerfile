# Hotfix Server licensing API, as deployed on Railway (see .railway/railway.ts).
FROM node:24-slim
WORKDIR /app
ENV NODE_ENV=production
COPY package.json package-lock.json ./
RUN npm ci --omit=dev
COPY src ./src
COPY migrations ./migrations
USER node
CMD ["node", "src/server.js"]
