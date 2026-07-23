;; Channels required to evaluate manifest.scm from a clean checkout.
(cons*
 (channel
  (name 'snamellit)
  (url "https://forge.snamellit.com/pti/snamguix.git")
  (branch "main")
  (commit "6ac31c06260313fbb270f086fbdcb8393bc40ef0")
  (introduction
   (make-channel-introduction
    "8e0f7748eeeb89cbfa3d9c1274ee555b7af868dd"
    (openpgp-fingerprint
     "C8AB C91C AB2D C195 B976 3877 DFAB C43C 55F2 E63A"))))
 %default-channels)
