-- modify "hosts" table
ALTER TABLE "hosts" ADD COLUMN "zone" character varying NOT NULL DEFAULT '';
