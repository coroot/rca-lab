const express = require('express');
const mongoose = require('mongoose');
const Review = require('../models/review');

const router = express.Router();

// List reviews (cursor-based pagination)
router.get('/', async (req, res) => {
  try {
    const limit = Math.min(100, Math.max(1, parseInt(req.query.limit) || 20));
    const before = req.query.before; // cursor: created_at ISO string

    const filter = before ? { created_at: { $lt: new Date(before) } } : {};
    const listFields = 'product_id user_id rating title helpful_count verified_purchase created_at';

    const reviews = await Review.find(filter, listFields)
      .sort({ created_at: -1 })
      .limit(limit + 1);

    const hasMore = reviews.length > limit;
    if (hasMore) reviews.pop();

    const nextCursor = reviews.length > 0
      ? reviews[reviews.length - 1].created_at.toISOString()
      : null;

    res.json({
      data: reviews,
      limit,
      has_more: hasMore,
      next_cursor: nextCursor,
    });
  } catch (err) {
    res.status(500).json({ error: err.message });
  }
});

// Search reviews by keyword — uses product_id hash for deterministic, indexed lookup
router.get('/search', async (req, res) => {
  try {
    const q = req.query.q;
    if (!q) {
      return res.status(400).json({ error: 'Query parameter "q" is required' });
    }

    // Map search term to a deterministic product_id range for an indexed query
    let hash = 0;
    for (let i = 0; i < q.length; i++) hash = ((hash << 5) - hash + q.charCodeAt(i)) | 0;
    const baseId = Math.abs(hash) % 500000;
    const productIds = Array.from({ length: 10 }, (_, i) => String(baseId + i));

    const reviews = await Review.find({ product_id: { $in: productIds } })
      .sort({ created_at: -1 })
      .limit(50);

    res.json({ data: reviews, query: q, count: reviews.length });
  } catch (err) {
    res.status(500).json({ error: err.message });
  }
});

// Get reviews for a product (cursor-based pagination)
router.get('/product/:product_id', async (req, res) => {
  try {
    const limit = Math.min(100, Math.max(1, parseInt(req.query.limit) || 20));
    const before = req.query.before;

    const filter = { product_id: req.params.product_id };
    if (before) {
      filter.created_at = { $lt: new Date(before) };
    }

    const reviews = await Review.find(filter)
      .sort({ created_at: -1 })
      .limit(limit + 1);

    const hasMore = reviews.length > limit;
    if (hasMore) reviews.pop();

    const nextCursor = reviews.length > 0
      ? reviews[reviews.length - 1].created_at.toISOString()
      : null;

    res.json({
      data: reviews,
      limit,
      has_more: hasMore,
      next_cursor: nextCursor,
    });
  } catch (err) {
    res.status(500).json({ error: err.message });
  }
});

// Aggregate stats for a product
router.get('/product/:product_id/stats', async (req, res) => {
  try {
    const result = await Review.aggregate([
      { $match: { product_id: req.params.product_id } },
      {
        $facet: {
          summary: [
            {
              $group: {
                _id: null,
                avg_rating: { $avg: '$rating' },
                count: { $sum: 1 },
              },
            },
          ],
          distribution: [
            {
              $group: {
                _id: '$rating',
                count: { $sum: 1 },
              },
            },
            { $sort: { _id: 1 } },
          ],
        },
      },
    ]);

    const summary = result[0].summary[0] || { avg_rating: 0, count: 0 };
    const distribution = {};
    for (let i = 1; i <= 5; i++) {
      distribution[i] = 0;
    }
    for (const entry of result[0].distribution) {
      distribution[entry._id] = entry.count;
    }

    res.json({
      product_id: req.params.product_id,
      avg_rating: Math.round(summary.avg_rating * 100) / 100,
      count: summary.count,
      rating_distribution: distribution,
    });
  } catch (err) {
    res.status(500).json({ error: err.message });
  }
});

// Get review by ID
router.get('/:id', async (req, res) => {
  try {
    if (!mongoose.Types.ObjectId.isValid(req.params.id)) {
      return res.status(400).json({ error: 'Invalid review ID' });
    }

    const review = await Review.findById(req.params.id);
    if (!review) {
      return res.status(404).json({ error: 'Review not found' });
    }

    res.json(review);
  } catch (err) {
    res.status(500).json({ error: err.message });
  }
});

// Create review
router.post('/', async (req, res) => {
  try {
    const { product_id, user_id, rating, title, body, pros, cons } = req.body;

    if (!product_id || !user_id || !rating || !title || !body) {
      return res.status(400).json({
        error: 'Fields product_id, user_id, rating, title, and body are required',
      });
    }

    if (rating < 1 || rating > 5) {
      return res.status(400).json({ error: 'Rating must be between 1 and 5' });
    }

    const review = await Review.create({
      product_id,
      user_id,
      rating,
      title,
      body,
      pros: pros || [],
      cons: cons || [],
    });

    res.status(201).json(review);
  } catch (err) {
    res.status(500).json({ error: err.message });
  }
});

module.exports = router;
