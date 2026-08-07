-- cluster_sync owns sync_events and sync_states. The built-in owner migration
-- creates the compatible GORM schema before this version is recorded.
SELECT 1;
