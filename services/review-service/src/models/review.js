const mongoose = require('mongoose');

const reviewSchema = new mongoose.Schema(
  {
    product_id: { type: String, required: true },
    user_id: { type: String, required: true },
    rating: { type: Number, required: true, min: 1, max: 5 },
    title: { type: String, required: true },
    body: { type: String, required: true },
    pros: { type: [String], default: [] },
    cons: { type: [String], default: [] },
    helpful_count: { type: Number, default: 0 },
    verified_purchase: { type: Boolean, default: false },
  },
  {
    timestamps: { createdAt: 'created_at', updatedAt: 'updated_at' },
  }
);

// Compound index for product reviews sorted by date
reviewSchema.index({ product_id: 1, created_at: -1 });
// Index for listing reviews sorted by date
reviewSchema.index({ created_at: -1 });

module.exports = mongoose.model('Review', reviewSchema);
