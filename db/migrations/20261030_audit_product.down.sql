DROP INDEX IF EXISTS idx_audit_product;
ALTER TABLE audit_logs DROP COLUMN IF EXISTS product_id;
