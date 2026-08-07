const express = require('express');
const mongoose = require('mongoose');
const pino = require('pino');
const reviewRoutes = require('./routes/reviews');

const logger = pino();

const app = express();
const PORT = process.env.PORT || 8080;
const MONGODB_URI =
  process.env.MONGODB_URI || 'mongodb://localhost:27017/reviews';

app.use(express.json());

app.get('/health', async (req, res) => {
  const dbState = mongoose.connection.readyState === 1 ? 'connected' : 'disconnected';
  res.json({ status: 'ok', db: dbState });
});

app.use('/reviews', reviewRoutes);

mongoose
  .connect(MONGODB_URI)
  .then(() => {
    logger.info('Connected to MongoDB');
    app.listen(PORT, () => {
      logger.info(`Review service listening on port ${PORT}`);
    });
  })
  .catch((err) => {
    logger.error(`Failed to connect to MongoDB: ${err.message}`);
    process.exit(1);
  });
