-- The product an audit line belongs to, recorded when it is written: the
-- audit page and the activity feed filter by product, and lines about a
-- seat, a device, an entitlement or a release file (or anything since
-- deleted) cannot be traced back to one afterwards. NULL on lines written
-- before this column, which the filter still finds the old way.
ALTER TABLE audit_logs ADD COLUMN IF NOT EXISTS product_id TEXT;
CREATE INDEX IF NOT EXISTS idx_audit_product ON audit_logs(product_id, created_at);
