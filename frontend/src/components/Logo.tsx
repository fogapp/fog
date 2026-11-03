// The mark: the house-and-cloud glyph from /fog.png, painted in the accent
// colour through a CSS mask so it follows every palette. It sits in a 520:456
// box, so the className sets the height and the width follows.
export default function Logo({ className = "h-5" }: { className?: string }) {
  return (
    <span
      aria-hidden="true"
      className={`inline-block shrink-0 aspect-[520/456] bg-accent ${className}`}
      style={{
        maskImage: "url(/fog.png)",
        WebkitMaskImage: "url(/fog.png)",
        maskSize: "contain",
        WebkitMaskSize: "contain",
        maskRepeat: "no-repeat",
        WebkitMaskRepeat: "no-repeat",
        maskPosition: "center",
        WebkitMaskPosition: "center",
      }}
    />
  );
}
