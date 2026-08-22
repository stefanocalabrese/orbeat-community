export default function HexLogo({ size = 23, className = "" }: { size?: number; className?: string }) {
  return (
    <svg width={size} height={size} viewBox="0 0 24 24" aria-hidden="true" className={className}>
      <polygon
        points="12,3 20,7.5 20,16.5 12,21 4,16.5 4,7.5"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.9"
        strokeLinejoin="round"
      />
      <circle cx="12" cy="3" r="2.5" fill="#a24bff" />
      <circle cx="12" cy="21" r="2.5" fill="#3d7bff" />
    </svg>
  );
}
