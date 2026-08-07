const express = require('express');
const mongoose = require('mongoose');
const pino = require('pino');
const reviewRoutes = require('./routes/reviews');

const logger = pino();

const app = express();
const PORT = process.env.PORT || 8080;
const MONGODB_URI =
  process.env.MONGODB_URI || 'mongodb://localhost:27017/reviews';
// Credentials are passed as driver options rather than embedded in the URI:
// the operator-generated password may contain characters that break URI
// parsing. When MONGO_USER/MONGO_PASSWORD are set, MONGODB_URI carries no
// credentials.
const mongoOptions = {};
if (process.env.MONGO_USER) {
  mongoOptions.auth = {
    username: process.env.MONGO_USER,
    password: process.env.MONGO_PASSWORD,
  };
  if (process.env.MONGO_AUTH_SOURCE) {
    mongoOptions.authSource = process.env.MONGO_AUTH_SOURCE;
  }
}

app.use(express.json());

app.get('/health', async (req, res) => {
  const dbState = mongoose.connection.readyState === 1 ? 'connected' : 'disconnected';
  res.json({ status: 'ok', db: dbState });
});

app.use('/reviews', reviewRoutes);

mongoose
  .connect(MONGODB_URI, mongoOptions)
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
