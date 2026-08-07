-- Core owns users, subscriptions, devices, plans, payments, promo codes and
-- related lifecycle tables. The transitional GORM owner migration is invoked
-- by the plugin DB runner before this immutable version marker is recorded.
SELECT 1;
