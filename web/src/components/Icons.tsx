import type { ReactNode, SVGProps } from 'react';

type P = SVGProps<SVGSVGElement> & { size?: number };

function Svg({ size = 14, children, ...rest }: P & { children: ReactNode }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={1.6}
      strokeLinecap="round"
      strokeLinejoin="round"
      className="i"
      aria-hidden="true"
      {...rest}
    >
      {children}
    </svg>
  );
}

export const BrandMark = ({ size = 18 }: { size?: number }) => (
  <svg
    width={size}
    height={size}
    viewBox="0 0 24 24"
    fill="none"
    aria-hidden="true"
    className="brand-mark-svg"
  >
    <path d="M5.6 7.8a8.2 8.2 0 0 1 12.8 0" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" />
    <path
      d="M18.4 16.2a8.2 8.2 0 0 1-12.8 0"
      stroke="currentColor"
      strokeOpacity="0.42"
      strokeWidth="1.7"
      strokeLinecap="round"
    />
    <path d="M7.8 12A4.2 4.2 0 1 1 12 16.2" stroke="var(--accent)" strokeWidth="1.7" strokeLinecap="round" />
    <circle cx="12" cy="12" r="1.7" fill="currentColor" />
    <circle cx="18.4" cy="7.8" r="1.5" fill="var(--accent)" />
  </svg>
);

export const BrandWordmark = () => (
  <b className="brand-name">
    hebi8<span className="brand-slash">/</span>orbit
  </b>
);

export const PAIcon = (p: P) => (
  <Svg {...p}>
    <circle cx="12" cy="12" r="3.2" />
    <path d="M12 3v2.5M12 18.5V21M3 12h2.5M18.5 12H21" />
  </Svg>
);
export const SwarmIcon = (p: P) => (
  <Svg {...p}>
    <circle cx="12" cy="12" r="2.2" />
    <circle cx="5" cy="7" r="1.6" />
    <circle cx="19" cy="7" r="1.6" />
    <circle cx="12" cy="20" r="1.6" />
    <path d="M6.4 8l3.8 2.8M17.6 8l-3.8 2.8M12 14.2v4.2" />
  </Svg>
);
export const DiffIcon = (p: P) => (
  <Svg {...p}>
    <path d="M8 4v10M3 9h10M14 18h7" />
    <circle cx="17" cy="7" r="3" />
  </Svg>
);
export const ArtIcon = (p: P) => (
  <Svg {...p}>
    <rect x="3.5" y="4.5" width="17" height="15" rx="2" />
    <path d="M3.5 9h17M9 9v10.5" />
  </Svg>
);
export const BranchIcon = (p: P) => (
  <Svg {...p}>
    <circle cx="6" cy="6" r="2" />
    <circle cx="6" cy="18" r="2" />
    <circle cx="18" cy="8" r="2" />
    <path d="M6 8v8M18 10c0 4-6 3-10 6" />
  </Svg>
);
export const CursorIcon = (p: P) => (
  <Svg {...p}>
    <path d="M12 3l8 4.5v9L12 21l-8-4.5v-9z" />
    <path d="M12 12l8-4.5M12 12v9M12 12L4 7.5" />
  </Svg>
);
export const VSCodeIcon = (p: P) => (
  <Svg {...p}>
    <path d="m17 3 4 2v14l-4 2-9-8.5L4 16l-1.5-1.2L6.5 12 2.5 9.2 4 8l4 3.5Z" />
  </Svg>
);
export const AntigravityIcon = (p: P) => (
  <Svg {...p}>
    <path d="M12 3 4.5 20h3.2l1.8-4.2h5L16.3 20h3.2Z" />
    <path d="M10.4 13 12 9.2 13.6 13Z" />
  </Svg>
);
export const ZedIcon = (p: P) => (
  <Svg {...p}>
    <path d="M5 5h14L7 19h12" />
    <path d="M15 5v4M9 15v4" />
  </Svg>
);
export function IdeGlyph({ scheme, size = 14 }: { scheme: string; size?: number }) {
  switch (scheme) {
    case 'vscode':
      return <VSCodeIcon size={size} />;
    case 'antigravity':
      return <AntigravityIcon size={size} />;
    case 'zed':
      return <ZedIcon size={size} />;
    default:
      return <CursorIcon size={size} />;
  }
}
export const StopSquareIcon = (p: P) => (
  <Svg {...p}>
    <rect x="7" y="7" width="10" height="10" rx="1.8" fill="currentColor" stroke="none" />
  </Svg>
);
export const QueueSendIcon = (p: P) => (
  <Svg {...p} strokeWidth={1.8}>
    <path d="M5 6h10M5 11h6" />
    <path d="M15 19v-7" />
    <path d="m11.5 15.5 3.5-3.5 3.5 3.5" />
  </Svg>
);
export const CardIcon = (p: P) => (
  <Svg {...p}>
    <rect x="3.5" y="4.5" width="17" height="15" rx="2.5" />
    <path d="M14.5 4.5v15" />
  </Svg>
);
export const SteerIcon = (p: P) => (
  <Svg {...p}>
    <path d="M13 3L5 14h6l-1 7 8-11h-6z" />
  </Svg>
);
export const SendUpIcon = (p: P) => (
  <Svg {...p} strokeWidth={2}>
    <path d="M12 19V5M6 11l6-6 6 6" />
  </Svg>
);
export const CaretIcon = (p: P) => (
  <Svg size={12} {...p}>
    <path d="M9 6l6 6-6 6" />
  </Svg>
);
export const MoonIcon = (p: P) => (
  <Svg {...p}>
    <path d="M20 14.5A8 8 0 1 1 9.5 4a6.5 6.5 0 0 0 10.5 10.5z" />
  </Svg>
);
export const SunIcon = (p: P) => (
  <Svg {...p}>
    <circle cx="12" cy="12" r="4" />
    <path d="M12 2.5v2M12 19.5v2M2.5 12h2M19.5 12h2M5.3 5.3l1.4 1.4M17.3 17.3l1.4 1.4M5.3 18.7l1.4-1.4M17.3 6.7l1.4-1.4" />
  </Svg>
);
export const DirectLocalIcon = (p: P) => (
  <Svg size={13} {...p}>
    <rect x="3" y="4" width="18" height="12" rx="2" />
    <path d="M2 20h20" />
    <circle cx="12" cy="10" r="1.6" fill="currentColor" stroke="none" />
  </Svg>
);
export const PermAutoIcon = (p: P) => (
  <Svg size={13.5} {...p}>
    <path d="M12 3 5 6v6c0 4.5 3 7.5 7 9 4-1.5 7-4.5 7-9V6Z" />
    <path d="M12 3v18" strokeOpacity={0.75} />
    <path d="M12 5.2 6.8 7.5V12c0 3.4 2.1 5.8 5.2 7.1Z" fill="currentColor" fillOpacity={0.28} stroke="none" />
  </Svg>
);
export const PermAskIcon = (p: P) => (
  <Svg size={13.5} {...p}>
    <path d="M12 3 5 6v6c0 4.5 3 7.5 7 9 4-1.5 7-4.5 7-9V6Z" />
    <path d="M9.8 9.8a2.2 2.2 0 1 1 3.3 1.9c-.8.4-1.1.8-1.1 1.5" />
    <circle cx="12" cy="16" r="0.7" fill="currentColor" stroke="none" />
  </Svg>
);
export const PermPlanIcon = (p: P) => (
  <Svg size={13.5} {...p}>
    <path d="M14 3H6a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V9Z" />
    <path d="M14 3v6h6" />
    <path d="M8 13h8M8 17h5" />
  </Svg>
);
export const PermYoloIcon = (p: P) => (
  <Svg size={13.5} {...p}>
    <path d="M12 3 5 6v6c0 4.5 3 7.5 7 9 4-1.5 7-4.5 7-9V6Z" />
    <path d="M13 7.5 9.2 12.6h2.8l-.8 4 3.8-5.1h-2.8Z" fill="currentColor" stroke="none" />
  </Svg>
);
