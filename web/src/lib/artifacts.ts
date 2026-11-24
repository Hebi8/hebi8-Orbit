import type {
  AnnotationView,
  BlobManifestItem,
  DeliveryView,
  TaskTimelineResponse,
} from '../generated/protocol';
import { basename, classifyArtifactKind, isImagePath } from './format';

export interface SessionArtifactItem {
  artifactId: string;
  kind: string;
  title: string;
  locator: string;
  deliveryId: string;
  source: 'ryugi' | 'blob';
  openUrl?: string;
  blobSha256?: string;
  sizeBytes?: number;
  createdAt: number;
  openAnnotations: number;
}

export function deliveryInlineImages(d: DeliveryView): BlobManifestItem[] {
  return (d.manifest.blobs ?? []).filter((b) => b.path && isImagePath(b.path));
}

export function deliveryReusableArtifacts(
  d: DeliveryView,
  annotations: AnnotationView[] = [],
): SessionArtifactItem[] {
  const out: SessionArtifactItem[] = [];
  for (const rb of d.ryugiBindings ?? []) {
    const count = annotations.filter(
      (a) => a.status === 'open' && a.deliveryId === d.deliveryId && a.source === 'ryugi',
    ).length;
    out.push({
      artifactId: `ryugi:${rb.bindingId}`,
      kind: classifyArtifactKind(rb.artifactLocator),
      title: basename(rb.artifactLocator) || rb.artifactLocator,
      locator: rb.artifactLocator,
      deliveryId: d.deliveryId,
      source: 'ryugi',
      openUrl: rb.openUrl,
      createdAt: rb.updatedAt || d.createdAt,
      openAnnotations: count,
    });
  }
  for (const b of d.manifest.blobs ?? []) {
    if (!b.path || isImagePath(b.path)) continue;
    const count = annotations.filter(
      (a) =>
        a.status === 'open' &&
        a.deliveryId === d.deliveryId &&
        (a.anchor.artifact === b.path || a.anchor.path === b.path),
    ).length;
    out.push({
      artifactId: `blob:${b.sha256}:${b.path}`,
      kind: classifyArtifactKind(b.path),
      title: basename(b.path) || b.path,
      locator: b.path,
      deliveryId: d.deliveryId,
      source: 'blob',
      blobSha256: b.sha256,
      sizeBytes: b.size,
      createdAt: d.createdAt,
      openAnnotations: count,
    });
  }
  return out;
}

/** Collect all deduplicated reusable Artifacts across a session timeline (newest first). */
export function collectSessionArtifacts(tl?: TaskTimelineResponse): SessionArtifactItem[] {
  if (!tl) return [];
  const formal = [...tl.deliveries].filter((d) => !d.lateArrival).reverse();
  const seen = new Set<string>();
  const out: SessionArtifactItem[] = [];
  for (const d of formal) {
    for (const item of deliveryReusableArtifacts(d, tl.annotations)) {
      const dedupKey = `${item.source}:${item.locator}`;
      if (seen.has(dedupKey)) continue;
      seen.add(dedupKey);
      out.push(item);
    }
  }
  return out;
}
